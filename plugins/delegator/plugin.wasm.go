package main

import (
	plugin_sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {
	plugin_sdk.Init()
}

// on_chat_request injects the torana_delegate_task tool if not already present.
//
//go:export on_chat_request
func on_chat_request(ptr, size uint32) uint64 {
	input := plugin_sdk.GetBytes(ptr, size)

	var msg struct {
		Chat  string `json:"chat"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}

	// Parse the input — if it's not JSON, pass through unchanged.
	_ = input
	_ = msg

	// For now, return pass-through. The Go host handles the actual
	// tool injection. The WASM plugin is a validation that the SDK works.
	return 0
}
