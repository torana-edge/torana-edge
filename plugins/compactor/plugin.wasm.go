// The compactor shrinks large tool results by delegating extraction to a
// cheap model (torana_offload_completion), guided by the intent captured by
// the intent plugin: "given what the agent was trying to find out, keep only
// the relevant parts of this output". Compacted results are cached by
// tool_call_id, so later turns replaying the same result reuse the compact
// form for free — that's where the savings compound.
//
// Run it AFTER the intent plugin. It is an alternative to keyword_compactor
// (deterministic, local, no model call) — pick ONE per deployment; both
// consume the same intent cache.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/torana-edge/torana-edge/sdk/pb"
	sdk "github.com/torana-edge/torana-edge/sdk/plugin-sdk"
)

func main() {}

const (
	intentCacheKey  = "intent"
	compactionCache = "compacted"
	minOffloadChars = 2000
)

// maxOffloadInputChars caps how much of a tool output is sent to the cheap
// summarizer. 0 (the default) means UNBOUNDED — the complete tool output is
// sent. A positive value is opt-in via plugins.config.compactor and truncates
// head+tail to that many chars. Loaded once, lazily, from the plugin config.
var (
	cfgOnce              sync.Once
	maxOffloadInputChars int
)

func loadConfig() {
	cfgOnce.Do(func() {
		var c struct {
			MaxOffloadInputChars int `json:"max_offload_input_chars"`
		}
		if raw := sdk.PluginConfig(); raw != "" {
			_ = json.Unmarshal([]byte(raw), &c)
		}
		if c.MaxOffloadInputChars > 0 {
			maxOffloadInputChars = c.MaxOffloadInputChars
		}
	})
}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		if !compactToolResults(ctx, req) {
			return nil, nil
		}
		return req, nil
	})
}

// ==========================================================================
// Tool result compaction
// ==========================================================================

func compactToolResults(ctx context.Context, req *pb.ChatRequest) bool {
	loadConfig()
	modified := false

	// Protect fresh tool results (Issue #166)
	lastAssistantIdx := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "assistant" {
			lastAssistantIdx = i
			break
		}
	}

	for i, msg := range req.Messages {
		if msg.Role != "tool" || msg.ToolCallId == "" || len(msg.Content) < minOffloadChars {
			continue
		}

		// Protect fresh unconsumed results.
		if i > lastAssistantIdx {
			continue
		}

		// Phase 0 observability: every result big enough to compact.
		sdk.EmitMetric("torana_compact_eligible_total", sdk.MetricCounter, 1, map[string]string{"tool": msg.ToolName})

		// Compaction cache check.
		cached, _ := sdk.HostCall("env.cache_get", compactionCache+":"+msg.ToolCallId)
		if cached != "" {
			recordSavings(len(msg.Content), len(cached))
			msg.Content = cached
			modified = true
			continue
		}

		// Get intent from cache (written by the intent plugin).
		intent, _ := sdk.HostCall("env.cache_get", intentCacheKey+":"+msg.ToolCallId)
		if intent == "" {
			// Eligible but no captured intent — the money left on the table.
			sdk.EmitMetric("torana_intent_missing_total", sdk.MetricCounter, 1, map[string]string{"tool": msg.ToolName})
			continue
		}

		// Extract conversation context.
		ctxStr := extractConversationContext(req.Messages, msg.ToolCallId)

		// Call cheap model for summarization.
		payload, _ := json.Marshal(map[string]any{
			"system_prompt": "You are a tool output summarizer. Given a tool output and an extraction intent, return ONLY the relevant parts. Be concise. Do not add commentary.",
			"user_prompt":   fmt.Sprintf("Intent: %s\n\nConversation context:\n%s\n\nTool output:\n%s\n\nExtract only the parts relevant to the intent.", intent, ctxStr, truncateForPrompt(msg.Content, maxOffloadInputChars)),
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

		recordSavings(len(msg.Content), len(offloadResp.Completion))
		msg.Content = offloadResp.Completion
		modified = true

		// Store in compaction cache.
		sdk.HostCall("env.cache_set", fmt.Sprintf(`{"key":"%s:%s","value":%q}`, compactionCache, msg.ToolCallId, offloadResp.Completion))
	}
	return modified
}

// recordSavings reports compaction byte savings to /stats via the host.
func recordSavings(originalBytes, finalBytes int) {
	sdk.HostCall("torana_record_savings",
		fmt.Sprintf(`{"original_bytes":%d,"final_bytes":%d}`, originalBytes, finalBytes))
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

// truncateForPrompt bounds the tool output sent to the summarizer. maxChars <= 0
// means unbounded: the complete output is sent (the default). A positive cap
// keeps the head and tail (first + last maxChars/2), where signal tends to
// cluster, dropping the middle.
func truncateForPrompt(content string, maxChars int) string {
	if maxChars <= 0 || len(content) <= maxChars {
		return content
	}
	half := maxChars / 2
	return content[:half] + "\n\n... [truncated] ...\n\n" + content[len(content)-half:]
}
