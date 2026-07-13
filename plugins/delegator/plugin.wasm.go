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

		model, _ := fullReq["Model"].(string)
		if model == "" {
			fullReq["Model"] = "claude-3-5-sonnet-20241022"
			chatBytes, _ := json.Marshal(fullReq)
			wrapper.Chat = string(chatBytes)
			return json.Marshal(wrapper)
		}
		return nil, nil
	})
}
