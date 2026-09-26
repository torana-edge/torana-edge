package mcpserver

import (
	"fmt"
	"strings"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
)

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
// identity and operator-configured MCP server names.
func (c *Correlator) ObserveResponse(response *engine.ChatResponse, shape, conversation string, servers []string, now time.Time) int {
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
		if id == "" && strings.HasPrefix(shape, "gemini") && response.ID != "" {
			id = fmt.Sprintf("gemini:%s:%d", response.ID, index)
		}
		if c.Record(tool, call.ArgumentsJSON, Binding{ConversationID: conversation, CallID: id}, now) {
			count++
		}
	}
	return count
}
