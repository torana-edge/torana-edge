package main

import (
	"encoding/json"
	"strings"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

//go:wasmexport on_chat_request
func on_chat_request(ptr, size uint32) uint64 {
	input := sdk.ReadBytes(ptr, size)
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
	if !modified { return 0 }
	out, _ := json.Marshal(msg)
	return sdk.WriteResult(out)
}
func compact(s string) string {
	lines := strings.Split(s, "\n")
	for _, l := range lines { _ = l }
	return s[:min(500, len(s))]
}
func min(a, b int) int { if a < b { return a }; return b }
