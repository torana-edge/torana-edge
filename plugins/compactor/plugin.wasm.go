package main

import (
	"encoding/json"
	"strings"

	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() { sdk.Init() }

//go:export alloc
func alloc(size uint32) uint32 { return sdk.Alloc(size) }

//go:export dealloc
func dealloc(ptr, size uint32) {}

//go:export on_chat_request
func on_chat_request(ptr, size uint32) uint64 {
	input := sdk.GetBytes(ptr, size)
	var msg struct {
		Chat     string `json:"chat"`
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if json.Unmarshal(input, &msg) != nil {
		return 0
	}
	modified := false
	for i := range msg.Messages {
		m := &msg.Messages[i]
		if m.Role != "tool" || len(m.Content) < 2000 {
			continue
		}
		compacted := compactDeterministic(m.Content)
		if compacted != m.Content && len(compacted) < len(m.Content) {
			m.Content = compacted
			modified = true
		}
	}
	if !modified {
		return 0
	}
	return sdk.WriteResult(msg)
}

func compactDeterministic(content string) string {
	indicators := []string{"func ", "type ", "import ", "package ", "//", "var ", "const ",
		"return ", "error", "log.", "config", "server", "plugin", "wasm"}
	lines := strings.Split(content, "\n")
	var kept []string
	for _, line := range lines {
		lower := strings.ToLower(line)
		for _, kw := range indicators {
			if strings.Contains(lower, kw) {
				kept = append(kept, line)
				break
			}
		}
	}
	if len(kept) == 0 {
		return content
	}
	if len(kept) > 150 {
		kept = kept[:150]
	}
	return strings.Join(kept, "\n")
}
