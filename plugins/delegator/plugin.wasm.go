package main

import (
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func init() {
	sdk.OnChatRequest(func(req map[string]any) (map[string]any, error) {
		req["handled_by"] = "delegator.wasm"
		return req, nil
	})
}

func main() {}
