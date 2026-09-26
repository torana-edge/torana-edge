package mcpserver

import "github.com/torana-edge/torana-edge/internal/engine"

// ToolPresence reports catalog evidence, not transport connectivity or consent.
// Search is the fixed discovery entry point. Unrelated server prefixes do not
// count, and requests without a catalog cannot prove that MCP is missing.
func ToolPresence(request *engine.ChatRequest, servers []string) string {
	if request == nil || len(request.Tools) == 0 {
		return "not_observed"
	}
	for _, tool := range request.Tools {
		if responseTool(tool.Name, servers) == "torana_search" {
			return "present"
		}
	}
	return "absent"
}
