// Minimal hand-rolled WASM fixture for raw runtime ABI tests.
// It implements the host ABI surface (alloc/dealloc/run_before_request)
// without the plugin SDK. The bump allocator is fine here — the fixture
// handles a handful of tiny test payloads, never production traffic.
package main

var heap [65536]byte
var bump uint32

//go:wasmexport alloc
func alloc(size uint32) uint32 {
	if bump+size > uint32(len(heap)) {
		return 0
	}
	ptr := bump
	bump += size
	return ptr
}

//go:wasmexport dealloc
func dealloc(ptr, size uint32) {}

//go:wasmexport run_before_request
func run_before_request(reqID uint64, ptr, size uint32) uint64 {
	// Return 0: "not handled" — the host keeps the original request.
	return 0
}

func main() {}
