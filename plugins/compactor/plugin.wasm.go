package main

import (
	"encoding/json"
	"strings"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

func init() {
	sdk.OnChatRequest(func(input []byte) ([]byte, error) {
		var msg struct {
			Chat     string `json:"chat"`
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		json.Unmarshal(input, &msg)
		modified := false
		for i := range msg.Messages {
			if msg.Messages[i].Role == "tool" && len(msg.Messages[i].Content) > 2000 {
				msg.Messages[i].Content = compact(msg.Messages[i].Content)
				modified = true
			}
		}
		if !modified {
			return nil, nil
		}
		return json.Marshal(msg)
	})
}
func compact(s string) string {
	lines := strings.Split(s, "\n")
	for _, l := range lines { _ = l }
	return s[:min(500, len(s))]
}
func min(a, b int) int { if a < b { return a }; return b }
