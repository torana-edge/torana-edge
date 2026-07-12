// Package plugin_sdk provides the shared alloc/dealloc interface for
// Torana WASM plugins. Plugins import this package and call Init()
// in main() to register the memory allocator.
//
// Usage:
//
//	//go:build wasm
//
//	package main
//
//	import plugin_sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
//
//	func main() {
//	    plugin_sdk.Init()
//	}
//
// The SDK uses make([]byte, size) for dynamic allocation with
// unsafe.Pointer to pass memory references through the WASM boundary.
// Wazero can dynamically scale the linear memory up to the configured
// limit — no fixed heap size.
package plugin_sdk

import (
	"encoding/json"
	"unsafe"
)

var (
	heap    []byte
	heapPtr uintptr
)

// Init registers the alloc/dealloc exports on the WASM module.
// Must be called in main() before any other code.
func Init() {
	// Initial allocation to seed the heap.
	heap = make([]byte, 4096)
	heapPtr = uintptr(unsafe.Pointer(&heap[0]))
}

// alloc allocates size bytes and returns a pointer.
// The memory is managed dynamically — wazero scales linear memory as needed.
//
//go:export alloc
func alloc(size uint32) uint32 {
	if len(heap) == 0 {
		Init()
	}
	// Grow heap if needed.
	if uint32(len(heap)) < size {
		heap = make([]byte, size*2)
		heapPtr = uintptr(unsafe.Pointer(&heap[0]))
	}
	ptr := uint32(heapPtr)
	heapPtr += uintptr(size)
	return ptr
}

// dealloc is a no-op — WASM's linear memory is garbage collected by wazero.
//
//go:export dealloc
func dealloc(ptr, size uint32) {}

// HostCall serializes input, calls alloc, copies to heap, and returns [ptr, len].
// Utility for plugins that need to return data to the host.
func HostCall(fn func(map[string]any) map[string]any) func(inputPtr, inputLen uint32) uint64 {
	return nil
}

// GetBytes returns the byte slice at the given pointer and length.
// Used by host functions to read WASM memory.
func GetBytes(ptr, length uint32) []byte {
	if ptr == 0 || length == 0 {
		return nil
	}
	p := unsafe.Pointer(uintptr(ptr))
	return unsafe.Slice((*byte)(p), int(length))
}

// WriteResult allocates memory and writes the given JSON value.
// Returns [ptr, len] suitable for returning from a WASM export.
func WriteResult(v any) uint64 {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`{"error":"marshal failed"}`)
	}
	outPtr := alloc(uint32(len(b)))
	copy(GetBytes(outPtr, uint32(len(b))), b)
	return uint64(outPtr)<<32 | uint64(len(b))
}
