// The intent plugin captures WHY the model makes each tool call.
//
// Request side, it teaches the convention: every tool schema gains a required
// "i" property ("what question are you answering?"), reinforced by a system
// prompt addendum that embeds a one-line example transcript. No synthetic
// messages are injected — a fake conversation is indistinguishable from real
// history and measurably contaminates behavior (verbatim intent leaks,
// topic-anchored refusals; see the Jul 16 experiments). Response side, it
// buffers the streamed tool-call arguments, extracts the "i" value into the
// shared cross-request cache (keyed by tool_call_id), and strips "i" back off
// so the agent harness never sees it.
//
// It exists as its own plugin so the compactors are independent consumers:
// run "intent" plus EITHER keyword_compactor (deterministic, local) OR
// compactor (cheap-model offload) — both read the same intent cache.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

const (
	intentField    = "i"
	intentCacheKey = "intent"
)

func init() {
	// ── Request side: teach the "i" convention ──────────────────────
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		if len(req.Tools) == 0 {
			return nil, nil
		}
		modified := injectIntentSchema(req)
		modified = injectSystemPrompt(req) || modified
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
				sdk.HostCall("env.cache_set", fmt.Sprintf(`{"key":"%s:%s","value":%q}`, intentCacheKey, toolID, intent))
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

		hadI := false
		if _, exists := props[intentField]; exists {
			hadI = true
		}

		props[intentField] = map[string]any{
			"type": "string",
			// The example-carrying description measured markedly better than
			// an abstract instruction (75% vs 54% goal-tied intents in the
			// Jul 16 experiments).
			"description": "the underlying question this call helps answer, NOT the action taken. " +
				"Good: 'where is the user locale mapped to a currency, to find the EU bug'. " +
				"Bad: 'reading currency.ts'.",
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

		if hadI {
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"hadI:%s","value":"true"}`, tool.Name))
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
