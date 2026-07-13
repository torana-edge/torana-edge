package main

import (
	"encoding/json"
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

		toolsAny, _ := fullReq["Tools"].([]any)
		modified := false
		for _, tAny := range toolsAny {
			t, _ := tAny.(map[string]any)
			if t == nil { continue }
			paramsAny, _ := t["Parameters"].(map[string]any)
			if paramsAny == nil { continue }
			propsAny, _ := paramsAny["properties"].(map[string]any)
			if propsAny == nil { continue }
			if _, ok := propsAny["i"]; !ok {
				propsAny["i"] = map[string]any{"type": "string", "description": "what you intend to accomplish"}
				reqAny, ok := paramsAny["required"].([]any)
				if ok {
					paramsAny["required"] = append(reqAny, "i")
				}
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
