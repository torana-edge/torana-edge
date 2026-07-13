package main

import (
	"encoding/json"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

//go:wasmexport on_chat_request
func on_chat_request(ptr, size uint32) uint64 {
	input := sdk.ReadBytes(ptr, size)
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
	if !modified { return 0 }
	out, _ := json.Marshal(msg)
	return sdk.WriteResult(out)
}
