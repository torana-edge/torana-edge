package main

import (
	"encoding/json"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

//go:wasmexport on_chat_request
func on_chat_request(ptr, size uint32) uint64 {
	input := sdk.ReadBytes(ptr, size)
	var msg map[string]any
	json.Unmarshal(input, &msg)
	msg["handled_by"] = "delegator.wasm"
	out, _ := json.Marshal(msg)
	return sdk.WriteResult(out)
}
