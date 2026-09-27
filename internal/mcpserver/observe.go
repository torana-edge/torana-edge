package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// TranscriptInvocation is a completed Torana tool call carried by the latest
// tool exchange in a request transcript. The host still applies namespace
// policy and input validation before doing anything with it.
type TranscriptInvocation struct {
	CallID string
	Input  json.RawMessage
	Ticket string
}

// TranscriptInvocations returns only completed torana_invoke calls from the
// latest tool exchange. This is the fallback for MCP transports that could not
// be correlated while the out-of-band call was live. Older exchanges are not
// replayed, and a reused call ID with contradictory arguments fails closed.
func TranscriptInvocations(request *engine.ChatRequest, servers []string) []TranscriptInvocation {
	if request == nil {
		return nil
	}
	resultTickets := map[string]string{}
	lastResult := -1
	for i := len(request.Messages) - 1; i >= 0; i-- {
		for _, block := range request.Messages[i].Blocks {
			if block.ToolResult != nil && block.ToolResult.ToolCallID != "" {
				if ticket := transcriptTicket(block.ToolResult); ticket != "" {
					resultTickets[block.ToolResult.ToolCallID] = ticket
				}
				if lastResult < 0 {
					lastResult = i
				}
			}
		}
		if request.Messages[i].Role == engine.RoleAssistant && lastResult < 0 {
			return nil
		}
		if lastResult >= 0 && request.Messages[i].Role == engine.RoleAssistant {
			seen := map[string]json.RawMessage{}
			conflict := map[string]bool{}
			for _, block := range request.Messages[i].Blocks {
				call := block.ToolUse
				if call == nil || call.ID == "" || resultTickets[call.ID] == "" || responseTool(call.Name, servers) != "torana_invoke" || call.InvocationKind == engine.ToolInvocationFreeform {
					continue
				}
				raw := json.RawMessage(call.Arguments.Bytes())
				if prior, exists := seen[call.ID]; exists && !bytes.Equal(prior, raw) {
					conflict[call.ID] = true
					continue
				}
				seen[call.ID] = raw
			}
			out := make([]TranscriptInvocation, 0, len(seen))
			for _, block := range request.Messages[i].Blocks {
				call := block.ToolUse
				if call == nil || conflict[call.ID] {
					continue
				}
				raw, exists := seen[call.ID]
				if !exists {
					continue
				}
				out = append(out, TranscriptInvocation{CallID: call.ID, Input: append(json.RawMessage(nil), raw...), Ticket: resultTickets[call.ID]})
				delete(seen, call.ID)
			}
			return out
		}
	}
	return nil
}

func transcriptTicket(result *engine.ToolResultBlock) string {
	if result == nil || result.IsError != nil && *result.IsError {
		return ""
	}
	for _, content := range result.Content {
		var output struct {
			OK     bool   `json:"ok"`
			Status string `json:"status"`
			Ticket string `json:"ticket"`
		}
		if json.Unmarshal([]byte(content.Text), &output) == nil && output.OK && output.Status == "pending" && output.Ticket != "" {
			return output.Ticket
		}
	}
	return ""
}

// responseTool accepts only the configured Torana server's names, not every
// server with a similarly named tool. Bare names are used by native MCP clients.
func responseTool(name string, servers []string) string {
	tool := canonicalTool(name)
	if tool == "" {
		return ""
	}
	if name == tool {
		return tool
	}
	for _, server := range servers {
		if server != "" && (name == "mcp__"+server+"__"+tool || name == server+"__"+tool) {
			return tool
		}
	}
	return ""
}

// ObserveResponse records complete calls in the canonical response after the
// client-facing adapter/bridge boundary. It does not mutate response bytes or
// accept identity from tool arguments. The caller supplies trusted request
// identity, a unique host request ID and operator-configured MCP server names.
func (c *Correlator) ObserveResponse(response *engine.ChatResponse, shape, conversation, requestID string, servers []string, now time.Time) int {
	if response == nil || response.Message == nil || response.UpstreamStatus < 200 || response.UpstreamStatus >= 300 {
		return 0
	}
	switch response.FinishReason {
	case "stop", "tool_calls", "tool_use", "end_turn", "STOP":
	default:
		return 0 // Incomplete or failed responses provide no binding evidence.
	}
	switch shape {
	case "anthropic", "openai-chat", "openai-responses", "gemini", "gemini-codeassist":
	default:
		return 0
	}
	count := 0
	for index, block := range response.Message.Blocks {
		call := block.ToolCall
		if call == nil {
			continue
		}
		tool := responseTool(call.Name, servers)
		if tool == "" {
			continue
		}
		id := call.ID
		if strings.HasPrefix(shape, "gemini") && (id == "" || id == call.Name) {
			// Native Gemini parsers historically substitute the function name
			// for missing IDs. That is not unique across calls or requests.
			id = ""
			if requestID != "" {
				id = fmt.Sprintf("gemini:%s:%d", requestID, index)
			}
		}
		if c.Record(tool, call.ArgumentsJSON, Binding{ConversationID: conversation, CallID: id}, now) {
			count++
		}
	}
	return count
}
