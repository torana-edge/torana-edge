// The intent plugin captures WHY the model makes each tool call.
//
// Request side, it teaches the convention: every tool schema gains a required
// "i" property ("what question are you answering?"), reinforced by a system
// prompt addendum that embeds a one-line example transcript. No synthetic
// messages are injected — a fake conversation is indistinguishable from real
// history and measurably contaminates behavior (verbatim intent leaks,
// topic-anchored refusals; see the Jul 16 experiments). It also keeps the
// model's own history consistent: prior tool calls get their captured "i"
// restored (rehydration), and never-captured ones get a heuristic fill —
// without this the model imitates its "i"-stripped history and emission
// collapses per tool (see rehydrateHistoryIntents). Response side, it
// buffers the streamed tool-call arguments, extracts the "i" value into the
// shared cross-request cache (keyed by tool_call_id AND by tool name+args),
// and strips "i" back off so the agent harness never sees it.
//
// It exists as its own plugin so the compactors are independent consumers:
// run "intent" plus EITHER keyword_compactor (deterministic, local) OR
// compactor (cheap-model offload) — both read the same intent cache.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

const (
	intentField    = "i"
	intentCacheKey = "intent"

	// intentDescription is the example-carrying "i" description. It measured
	// markedly better than an abstract instruction (75% vs 54% goal-tied
	// intents in the Jul 16 experiments).
	intentDescription = "the underlying question this call helps answer, NOT the action taken. " +
		"Good: 'where is the user locale mapped to a currency, to find the EU bug'. " +
		"Bad: 'reading currency.ts'."
)

// fillMode controls what happens to a history tool call whose intent was never
// captured (the model organically omitted "i" — nothing to rehydrate):
// "heuristic" (default) fills it with a template derived from the call's own
// arguments and the current task; "off" leaves it untouched. Loaded once from
// plugins.config.intent.
var (
	cfgOnce  sync.Once
	fillMode = "heuristic"
)

func loadConfig() {
	cfgOnce.Do(func() {
		var c struct {
			Fill string `json:"fill"`
		}
		if raw := sdk.PluginConfig(); raw != "" {
			_ = json.Unmarshal([]byte(raw), &c)
		}
		if c.Fill != "" {
			fillMode = c.Fill
		}
	})
}

func init() {
	// ── Request side: teach the "i" convention ──────────────────────
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		if len(req.Tools) == 0 {
			return nil, nil
		}
		modified := injectIntentSchema(req)
		modified = injectSystemPrompt(req) || modified
		// Re-hydrate "i" onto the model's PRIOR tool calls in history. We
		// strip "i" before returning to the harness, so the harness replays
		// the model's own tool calls without it — and the model imitates that
		// stripped history, dropping "i" on new calls within a few turns
		// (measured: ~96% single-turn capture collapses to ~18% multi-turn).
		// Restoring "i" here (from the cache we populated when it was first
		// emitted) shows the model a consistent history, so it keeps emitting.
		modified = rehydrateHistoryIntents(req) || modified
		if !modified {
			return nil, nil
		}
		return req, nil
	})

	// ── Response side: extract intent ───────────────────────────────
	//
	// Argument deltas are buffered and suppressed; at ToolCallEnd the
	// assembled arguments (intent extracted, "i" stripped when Torana
	// injected it) are emitted as one complete ToolCallDelta followed
	// by the ToolCallEnd.
	sdk.OnStreamChunk(func(ctx context.Context, chunk *pb.StreamEvent) (*pb.StreamEventResult, error) {
		// Track tool call by index.
		if ts := chunk.GetToolCallStart(); ts != nil {
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"tool:%d","value":%q}`, ts.Index, ts.Id))
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"name:%d","value":%q}`, ts.Index, ts.Name))
			return sdk.Pass(), nil
		}

		// Buffer and suppress tool call argument fragments.
		if td := chunk.GetToolCallDelta(); td != nil {
			toolID, _ := sdk.HostCall("env.meta_get", fmt.Sprintf("tool:%d", td.Index))
			if toolID == "" {
				return sdk.Pass(), nil
			}
			key := "frag:" + toolID
			prev, _ := sdk.HostCall("env.meta_get", key)
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"%s","value":%q}`, key, prev+td.ArgumentsDelta))
			return sdk.Suppress(), nil
		}

		// On ToolCallEnd: extract intent, optionally strip "i", emit args.
		if te := chunk.GetToolCallEnd(); te != nil {
			toolID, _ := sdk.HostCall("env.meta_get", fmt.Sprintf("tool:%d", te.Index))
			toolName, _ := sdk.HostCall("env.meta_get", fmt.Sprintf("name:%d", te.Index))
			if toolID == "" {
				return sdk.Pass(), nil
			}
			key := "frag:" + toolID
			fullArgs, _ := sdk.HostCall("env.meta_get", key)
			// Clean up (empty value deletes the key).
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"%s","value":""}`, key))
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"tool:%d","value":""}`, te.Index))
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"name:%d","value":""}`, te.Index))

			if fullArgs == "" {
				// No fragments were buffered — nothing to re-emit.
				return sdk.Pass(), nil
			}

			emitArgs := func(args string) *pb.StreamEventResult {
				// The fragments were suppressed, so the complete arguments
				// MUST be emitted here even when unchanged.
				return sdk.Emit(
					&pb.StreamEvent{
						Event: &pb.StreamEvent_ToolCallDelta{
							ToolCallDelta: &pb.ToolCallDelta{
								Index:          te.Index,
								ArgumentsDelta: args,
							},
						},
					},
					chunk,
				)
			}

			var args map[string]any
			if !strings.HasPrefix(fullArgs, "{") || json.Unmarshal([]byte(fullArgs), &args) != nil {
				return emitArgs(fullArgs), nil
			}

			// Extract and cache intent. Phase 0 observability: count how
			// often the model actually follows the convention, per tool.
			labels := map[string]string{"tool": toolName}
			if intent, ok := args[intentField].(string); ok && intent != "" {
				// Key by tool_call_id (works when the harness echoes IDs, e.g.
				// most OpenAI clients) AND by tool name+args content. The
				// content key survives harnesses that reassign tool_call_ids
				// across turns (Claude Code does), which is the only key
				// rehydration can rely on since the response-stream ID never
				// reappears in later request history.
				sdk.HostCall("env.cache_set", fmt.Sprintf(`{"key":"%s:%s","value":%q}`, intentCacheKey, toolID, intent))
				sdk.HostCall("env.cache_set", fmt.Sprintf(`{"key":%q,"value":%q}`, contentKey(toolName, args), intent))
				sdk.EmitMetric("torana_intent_captured_total", sdk.MetricCounter, 1, labels)
				// Debug visibility for dogfooding: intent QUALITY (goal vs
				// action description) is only judgeable by reading the values.
				if len(intent) > 160 {
					intent = intent[:160] + "…"
				}
				sdk.Log(fmt.Sprintf("intent[%s %s]: %s", toolName, toolID, intent), sdk.LogLevelDebug)
			} else {
				sdk.EmitMetric("torana_intent_absent_total", sdk.MetricCounter, 1, labels)
				sdk.Log(fmt.Sprintf("intent[%s %s]: ABSENT", toolName, toolID), sdk.LogLevelDebug)
			}

			// Strip "i" if not originally in schema.
			if toolName != "" {
				hadIRaw, _ := sdk.HostCall("env.meta_get", "hadI:"+toolName)
				if hadIRaw != "true" {
					delete(args, intentField)
				}
			}

			modifiedJSON, err := json.Marshal(args)
			if err != nil {
				return emitArgs(fullArgs), nil
			}
			return emitArgs(string(modifiedJSON)), nil
		}

		return sdk.Pass(), nil
	})
}

// ==========================================================================
// History re-hydration
// ==========================================================================

// rehydrateHistoryIntents restores the "i" field onto the model's prior
// assistant tool calls in the conversation history, reading each intent back
// from the cross-request cache. This counters the model imitating its own
// "i"-stripped history. Calls whose intent was never captured are FILLED with
// a derived heuristic (unless fill is "off"): history "i" values act as
// few-shot examples, so a single "i"-less call becomes a self-reinforcing
// per-tool precedent (measured: one organic miss collapsed that tool's
// emission to 0 for the rest of the session), while presence — even a
// mediocre fill among real intents — sustains near-100% emission without
// dragging new-call quality down to the fill's level. A constant placeholder
// is NOT safe: models copy the literal value into new calls. Trailing
// reminder messages recovered only ~70% in the same experiments and add
// contamination surface — kept as a fallback idea, not implemented.
func rehydrateHistoryIntents(req *pb.ChatRequest) bool {
	loadConfig()
	task := latestUserSnippet(req.Messages)
	restored, filled, present := 0, 0, 0
	modified := false
	for _, msg := range req.Messages {
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			continue
		}
		for _, tc := range msg.ToolCalls {
			var args map[string]any
			if len(tc.ArgumentsJson) == 0 {
				args = map[string]any{}
			} else if json.Unmarshal(tc.ArgumentsJson, &args) != nil {
				continue
			}
			if _, ok := args[intentField]; ok {
				present++
				continue // already carries "i"
			}
			// Look up by the content key (tool name + args). This is the only
			// key that survives harnesses reassigning tool_call_ids across
			// turns — the response-stream ID we cached under never reappears
			// in later request history.
			intent, _ := sdk.HostCall("env.cache_get", contentKey(tc.Name, args))
			if intent != "" {
				// Bridge the real intent to this request's own tool_call_id so
				// the compactors' intent:<tool_call_id> lookup (keyed off the
				// tool RESULT message) works on harnesses that reassign IDs.
				sdk.HostCall("env.cache_set", fmt.Sprintf(`{"key":"%s:%s","value":%q}`, intentCacheKey, tc.Id, intent))
				restored++
			} else {
				if fillMode == "off" {
					continue
				}
				// Filled values are injected into history only — never cached
				// and never bridged: the intent cache stays real-captured-only
				// so compaction quality is driven by real intents.
				intent = heuristicFill(tc.Name, args, task)
				sdk.EmitMetric("torana_intent_filled_total", sdk.MetricCounter, 1, map[string]string{"tool": tc.Name})
				sdk.Log(fmt.Sprintf("intent-fill[%s %s]: %s", tc.Name, tc.Id, intent), sdk.LogLevelDebug)
				filled++
			}
			args[intentField] = intent
			if b, err := json.Marshal(args); err == nil {
				tc.ArgumentsJson = b
				modified = true
			}
		}
	}
	if restored+filled > 0 {
		sdk.Log(fmt.Sprintf("rehydrate: %d restored, %d filled, %d already present", restored, filled, present), sdk.LogLevelDebug)
	}
	return modified
}

// heuristicFill derives a stand-in intent for a history tool call whose real
// intent was never captured. Its only job is presence — preventing an
// "i"-less precedent — but it carries the call's primary argument and the
// current task so it reads as a plausible (if mediocre) example rather than a
// literal token the model might copy verbatim.
func heuristicFill(name string, args map[string]any, task string) string {
	subject := name
	keys := make([]string, 0, len(args))
	for k := range args {
		if k != intentField {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := args[k].(string); ok && s != "" {
			subject = truncateRunes(s, 80)
			break
		}
	}
	out := "what " + subject + " shows"
	if task != "" {
		out += ", toward: " + task
	}
	return out
}

// latestUserSnippet returns a short single-line excerpt of the most recent
// user message, skipping harness-injected reminder blocks.
func latestUserSnippet(msgs []*pb.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != "user" || m.Content == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(m.Content), "<system-reminder>") {
			continue
		}
		return truncateRunes(strings.Join(strings.Fields(m.Content), " "), 80)
	}
	return ""
}

// truncateRunes shortens s to at most n runes, never splitting a rune.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// contentKey derives a cache key from a tool call's name and arguments,
// excluding "i". Go's json.Marshal sorts map keys, so the encoding is
// canonical: the response side (which strips "i") and the request side (where
// "i" is already absent) produce the same key for the same logical call.
// Collisions (same tool + args, different intent) resolve last-write-wins,
// which is acceptable for a hint.
func contentKey(name string, args map[string]any) string {
	cp := make(map[string]any, len(args))
	for k, v := range args {
		if k == intentField {
			continue
		}
		cp[k] = v
	}
	// Encode as a JSON array so the key stays JSON-safe (no control-char
	// separator that would break the cache_set payload) while remaining
	// canonical — Go sorts the inner map's keys.
	b, _ := json.Marshal([]any{name, cp})
	return "intentc:" + string(b)
}

// ==========================================================================
// Schema injection
// ==========================================================================

func injectIntentSchema(req *pb.ChatRequest) bool {
	modified := false
	for _, tool := range req.Tools {
		if len(tool.ParametersJson) == 0 {
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(tool.ParametersJson, &params); err != nil {
			continue
		}
		if params["type"] == nil {
			params["type"] = "object"
		}
		props, _ := params["properties"].(map[string]any)
		if props == nil {
			props = make(map[string]any)
			params["properties"] = props
		}

		// A tool that natively declares "i" (omp's tools do — the harness
		// adopted the intent field itself) keeps its structural contract:
		// required/optionality and additionalProperties are never touched,
		// and the response side never strips the value (that's what hadI
		// records). Only the DESCRIPTION is upgraded to the example-carrying
		// form — advisory prose, not contract, and measured markedly better
		// at producing goal-tied intents (omp's native "concise intent"
		// yielded action-labels like "Map repo structure", which starve the
		// compactors' keyword extraction).
		if existing, exists := props[intentField]; exists {
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"hadI:%s","value":"true"}`, tool.Name))
			if m, ok := existing.(map[string]any); ok {
				m["description"] = intentDescription
			}
		} else {
			props[intentField] = map[string]any{
				"type":        "string",
				"description": intentDescription,
			}

			required, _ := params["required"].([]any)
			found := false
			for _, r := range required {
				if s, ok := r.(string); ok && s == intentField {
					found = true
					break
				}
			}
			if !found {
				params["required"] = append(required, intentField)
			}
			params["additionalProperties"] = false
		}

		newJSON, err := json.Marshal(params)
		if err == nil && string(newJSON) != string(tool.ParametersJson) {
			tool.ParametersJson = newJSON
			modified = true
		}
	}
	return modified
}

// injectSystemPrompt appends the "i" convention with a one-line example
// TRANSCRIPT embedded in the system prompt — the winning strategy from the
// Jul 16 experiments: it matches few-shot messages on intent quality with
// zero conversation contamination and no per-request message overhead.
func injectSystemPrompt(req *pb.ChatRequest) bool {
	const addendum = "\n\nEvery tool call has an \"i\" field: the underlying question the call " +
		"helps answer, never the action taken. Example of a good call:\n" +
		"  read_file(path=\"src/pricing.ts\", i=\"Which table maps locale to currency, to find why EU shows USD\")\n" +
		"Example of a BAD value: i=\"reading pricing.ts\" (action description — discarded)."
	modified := false
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			msg.Content += addendum
			modified = true
			return modified
		}
	}
	req.Messages = append([]*pb.Message{{
		Role:    "system",
		Content: "[SYSTEM]" + addendum,
	}}, req.Messages...)
	return true
}
