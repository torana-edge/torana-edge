package plugin_sdk

import (
	"sync"
	"unsafe"
)

var (
	pinned   = make(map[uint32][]byte)
	pinMutex sync.Mutex
)

//go:wasmexport alloc
func alloc(size uint32) uint32 {
	buf := make([]byte, size)
	ptr := uint32(uintptr(unsafe.Pointer(&buf[0])))
	pinMutex.Lock()
	pinned[ptr] = buf
	pinMutex.Unlock()
	return ptr
}

//go:wasmexport dealloc
func dealloc(ptr uint32, size uint32) {
	pinMutex.Lock()
	delete(pinned, ptr)
	pinMutex.Unlock()
}

// ReadBytes reads from a pointer returned by alloc.
func ReadBytes(ptr, size uint32) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), int(size))
}

// WriteResult allocates memory, copies data, returns packed ptr|len.
func WriteResult(data []byte) uint64 {
	p := alloc(uint32(len(data)))
	copy(ReadBytes(p, uint32(len(data))), data)
	return uint64(p)<<32 | uint64(len(data))
}

var chatRequestHandler func(req []byte) ([]byte, error)

// OnChatRequest registers the handler for chat requests.
func OnChatRequest(handler func(req []byte) ([]byte, error)) {
	chatRequestHandler = handler
}

//go:wasmexport on_chat_request
func on_chat_request(ptr, size uint32) uint64 {
	if chatRequestHandler == nil {
		return 0
	}
	input := ReadBytes(ptr, size)
	out, err := chatRequestHandler(input)
	if err != nil || len(out) == 0 {
		return 0
	}
	return WriteResult(out)
}
