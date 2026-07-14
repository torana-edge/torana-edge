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
	intentField     = "i"
	intentCacheKey  = "intent"
	compactionCache = "compacted"
	minOffloadChars = 2000
)

func init() {
	// ── Request side: schema injection + tool result compaction ──────
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		modified := false

		// Phase 1: Inject "i" field into tool schemas.
		if len(req.Tools) > 0 {
			modified = injectIntentSchema(req) || modified
			modified = injectSystemPrompt(req) || modified
			modified = injectFewShot(req) || modified
		}

		// Phase 2: Compact tool results with model delegation.
		modified = compactToolResults(ctx, req) || modified

		if !modified {
			return nil, nil
		}
		return req, nil
	})

	// ── Response side: extract intent ───────────────────────────────
	sdk.OnStreamChunk(func(ctx context.Context, chunk *pb.StreamEvent) (*pb.StreamEvent, error) {
		// Track tool call by index.
		if ts := chunk.GetToolCallStart(); ts != nil {
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"tool:%d","value":%q}`, ts.Index, ts.Id))
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"name:%d","value":%q}`, ts.Index, ts.Name))
			return nil, nil
		}

		// Buffer tool call argument fragments.
		if td := chunk.GetToolCallDelta(); td != nil {
			toolID, _ := sdk.HostCall("env.meta_get", fmt.Sprintf("tool:%d", td.Index))
			if toolID == "" {
				return nil, nil
			}
			key := "frag:" + toolID
			prev, _ := sdk.HostCall("env.meta_get", key)
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"%s","value":%q}`, key, prev+td.ArgumentsDelta))
			return nil, nil
		}

		// On ToolCallEnd: extract intent, optionally strip "i".
		if te := chunk.GetToolCallEnd(); te != nil {
			toolID, _ := sdk.HostCall("env.meta_get", fmt.Sprintf("tool:%d", te.Index))
			toolName, _ := sdk.HostCall("env.meta_get", fmt.Sprintf("name:%d", te.Index))
			if toolID == "" {
				return nil, nil
			}
			key := "frag:" + toolID
			fullArgs, _ := sdk.HostCall("env.meta_get", key)
			// Clean up.
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"%s","value":""}`, key))
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"tool:%d","value":""}`, te.Index))
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"name:%d","value":""}`, te.Index))

			if fullArgs == "" || !strings.HasPrefix(fullArgs, "{") {
				return nil, nil
			}

			var args map[string]any
			if err := json.Unmarshal([]byte(fullArgs), &args); err != nil {
				return nil, nil
			}

			// Extract and cache intent.
			if intent, ok := args[intentField].(string); ok && intent != "" {
				sdk.HostCall("env.cache_set", fmt.Sprintf(`{"key":"%s:%s","value":%q}`, intentCacheKey, toolID, intent))
			}

			// Strip "i" if not originally in schema.
			if toolName != "" {
				hadIRaw, _ := sdk.HostCall("env.meta_get", "hadI:"+toolName)
				if hadIRaw != "true" {
					delete(args, intentField)
				}
			}

			modifiedJSON, _ := json.Marshal(args)
			if string(modifiedJSON) != fullArgs {
				return &pb.StreamEvent{
					Event: &pb.StreamEvent_ToolCallDelta{
						ToolCallDelta: &pb.ToolCallDelta{
							Index:          te.Index,
							ArgumentsDelta: string(modifiedJSON),
						},
					},
				}, nil
			}
			return nil, nil
		}

		return nil, nil
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
	last := len(req.Messages) - 1
	if last < 0 {
		return false
	}
	req.Messages = append(req.Messages[:last], append(fewShot, req.Messages[last:]...)...)
	return true
}

// ==========================================================================
// Tool result compaction
// ==========================================================================

func compactToolResults(ctx context.Context, req *pb.ChatRequest) bool {
	modified := false
	for _, msg := range req.Messages {
		if msg.Role != "tool" || msg.ToolCallId == "" || len(msg.Content) < minOffloadChars {
			continue
		}

		// Compaction cache check.
		cached, _ := sdk.HostCall("env.cache_get", compactionCache+":"+msg.ToolCallId)
		if cached != "" {
			msg.Content = cached
			modified = true
			continue
		}

		// Get intent from cache.
		intent, _ := sdk.HostCall("env.cache_get", intentCacheKey+":"+msg.ToolCallId)
		if intent == "" {
			continue
		}

		// Extract conversation context.
		ctxStr := extractConversationContext(req.Messages, msg.ToolCallId)

		// Call cheap model for summarization.
		payload, _ := json.Marshal(map[string]any{
			"system_prompt": "You are a tool output summarizer. Given a tool output and an extraction intent, return ONLY the relevant parts. Be concise. Do not add commentary.",
			"user_prompt":   fmt.Sprintf("Intent: %s\n\nConversation context:\n%s\n\nTool output:\n%s\n\nExtract only the parts relevant to the intent.", intent, ctxStr, truncateForPrompt(msg.Content, 14000)),
		})
		result, err := sdk.HostCall("torana_offload_completion", string(payload))
		if err != nil || result == "" {
			continue
		}
		var offloadResp struct {
			Status     string `json:"status"`
			Completion string `json:"completion"`
		}
		if json.Unmarshal([]byte(result), &offloadResp) != nil || offloadResp.Status != "ok" || offloadResp.Completion == "" {
			continue
		}

		msg.Content = offloadResp.Completion
		modified = true

		// Store in compaction cache.
		sdk.HostCall("env.cache_set", fmt.Sprintf(`{"key":"%s:%s","value":%q}`, compactionCache, msg.ToolCallId, offloadResp.Completion))
	}
	return modified
}

func extractConversationContext(msgs []*pb.Message, excludeToolCallID string) string {
	var parts []string
	collected := 0
	for i := len(msgs) - 1; i >= 0 && collected < 5; i-- {
		msg := msgs[i]
		if msg.ToolCallId == excludeToolCallID {
			continue
		}
		content := ""
		switch msg.Role {
		case "user":
			content = msg.Content
		case "assistant":
			if msg.Content != "" {
				content = msg.Content
			}
		case "tool":
			continue
		default:
			continue
		}
		if content != "" {
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			parts = append([]string{msg.Role + ": " + content}, parts...)
			collected++
		}
	}
	if len(parts) == 0 {
		return "no prior conversation context available"
	}
	return strings.Join(parts, "\n")
}

func truncateForPrompt(content string, maxChars int) string {
	if len(content) <= maxChars {
		return content
	}
	half := maxChars / 2
	return content[:half] + "\n\n... [truncated] ...\n\n" + content[len(content)-half:]
}
