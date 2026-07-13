package main

import (
	"encoding/json"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

func init() {
	sdk.OnChatRequest(func(input []byte) ([]byte, error) {
		var msg struct {
			Chat  string `json:"chat"`
			Tools []struct {
				Name       string         `json:"name"`
				Parameters map[string]any `json:"parameters"`
			} `json:"tools"`
		}
		json.Unmarshal(input, &msg)
		modified := false
		for i := range msg.Tools {
			t := &msg.Tools[i]
			if t.Parameters == nil { continue }
			p, _ := t.Parameters["properties"].(map[string]any)
			if p == nil { continue }
			if _, ok := p["i"]; !ok {
				p["i"] = map[string]any{"type": "string", "description": "what you intend to accomplish"}
				if r, ok := t.Parameters["required"].([]any); ok {
					t.Parameters["required"] = append(r, "i")
				}
				modified = true
			}
		}
		if !modified {
			return nil, nil
		}
		return json.Marshal(msg)
	})
}
