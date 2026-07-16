// The intent plugin captures WHY the model makes each tool call.
//
// Request side, it teaches the convention: every tool schema gains a required
// "i" property ("what question are you answering?"), reinforced by a system
// prompt addendum and a few-shot demonstration. Response side, it buffers the
// streamed tool-call arguments, extracts the "i" value into the shared
// cross-request cache (keyed by tool_call_id), and strips "i" back off so the
// agent harness never sees it.
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
		modified = injectFewShot(req) || modified
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
			} else {
				sdk.EmitMetric("torana_intent_absent_total", sdk.MetricCounter, 1, labels)
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
			"type":        "string",
			"description": "what you intend to accomplish: the question you are answering or the information you need",
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

func injectSystemPrompt(req *pb.ChatRequest) bool {
	const addendum = "\n\nWhen populating the \"i\" field on any tool call, do not describe the " +
		"action you are taking. Instead state the underlying question you are trying to " +
		"answer or decision you are trying to make. Action descriptions in \"i\" will be discarded."
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

func injectFewShot(req *pb.ChatRequest) bool {
	toolName := "read"
	if len(req.Tools) > 0 && req.Tools[0].Name != "" {
		toolName = req.Tools[0].Name
	}
	exampleArgs, _ := json.Marshal(map[string]any{
		"path": "server.go",
		"i":    "Understand error handling in the proxy pipeline, looking for 5xx responses",
	})
	fewShot := []*pb.Message{
		{Role: "user", Content: "I need to understand how the proxy handles upstream errors."},
		{Role: "assistant", ToolCalls: []*pb.ToolCall{{
			Id: "call_mock_fewshot_1", Name: toolName, ArgumentsJson: exampleArgs,
		}}},
		{Role: "tool", ToolCallId: "call_mock_fewshot_1", Content: "[few-shot example]"},
	}
	if len(req.Messages) == 0 {
		return false
	}
	// Insert directly after the leading system message(s). Inserting
	// mid-conversation can split an assistant tool_call from its tool
	// result, which strict providers reject with a 400
	// ("tool_calls must be followed by tool messages").
	insert := 0
	for insert < len(req.Messages) && req.Messages[insert].Role == "system" {
		insert++
	}
	out := make([]*pb.Message, 0, len(req.Messages)+len(fewShot))
	out = append(out, req.Messages[:insert]...)
	out = append(out, fewShot...)
	out = append(out, req.Messages[insert:]...)
	req.Messages = out
	return true
}
