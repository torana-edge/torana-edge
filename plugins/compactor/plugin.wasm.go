package main

import (
	"encoding/json"
	"strings"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

func init() {
	sdk.OnChatRequest(func(input []byte) ([]byte, error) {
		var wrapper struct {
			Chat string `json:"chat"`
		}
		json.Unmarshal(input, &wrapper)
		if wrapper.Chat == "" { return nil, nil }

		var fullReq map[string]any
		json.Unmarshal([]byte(wrapper.Chat), &fullReq)

		messagesAny, _ := fullReq["Messages"].([]any)
		modified := false
		for _, mAny := range messagesAny {
			m, _ := mAny.(map[string]any)
			if m == nil { continue }
			role, _ := m["Role"].(string)
			content, _ := m["Content"].(string)
			if role == "tool" && len(content) > 2000 {
				m["Content"] = compact(content)
				modified = true
			}
		}

		if !modified {
			return nil, nil
		}

		chatBytes, _ := json.Marshal(fullReq)
		wrapper.Chat = string(chatBytes)
		return json.Marshal(wrapper)
	})
}

func compact(s string) string {
	lines := strings.Split(s, "\n")
	for _, l := range lines { _ = l }
	return s[:min(500, len(s))]
}
func min(a, b int) int { if a < b { return a }; return b }
