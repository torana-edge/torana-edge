package main

import sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"

func main() { sdk.Init() }

//go:export alloc
func alloc(size uint32) uint32 { return sdk.Alloc(size) }

//go:export dealloc
func dealloc(ptr, size uint32) {}

//go:export on_chat_request
func on_chat_request(ptr, size uint32) uint64 {
	input := sdk.GetBytes(ptr, size)
	_ = input
	return 0 // pass-through: Go host handles tool injection
}
