package main

import (
	"encoding/json"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func init() {
	sdk.OnChatRequest(func(input []byte) ([]byte, error) {
		var msg map[string]any
		json.Unmarshal(input, &msg)
		msg["handled_by"] = "delegator.wasm"
		return json.Marshal(msg)
	})
}

func main() {}
