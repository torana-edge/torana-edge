//go:build wasip1

package main

import (
	"sync"
	"unsafe"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
)

func main() {}

// A HANDWRITTEN guest: records verdicts through host calls and then returns a
// malformed v2 frame.
//
// It cannot use the SDK, because the SDK's typed results make a malformed frame
// unrepresentable — which is the point of them. That leaves the host's
// decode-error branch reachable only by a guest like this, and that branch has
// to discard the plugin's non-block verdicts and still honour a recorded block.
// A test calling the host's helpers directly would stay green if the branch
// forgot either.

//go:wasmimport env host_call
func hostCall(cmdPtr, cmdLen, argsPtr, argsLen uint32) uint64

var (
	pinMu  sync.Mutex
	pinned = map[uint32][]byte{}
)

//go:wasmexport alloc
func allocExport(size uint32) uint32 {
	if size == 0 {
		return 0
	}
	buf := make([]byte, size)
	ptr := uint32(uintptr(unsafe.Pointer(&buf[0])))
	pinMu.Lock()
	pinned[ptr] = buf
	pinMu.Unlock()
	return ptr
}

//go:wasmexport dealloc
func deallocExport(ptr, size uint32) {
	pinMu.Lock()
	delete(pinned, ptr)
	pinMu.Unlock()
}

//go:wasmexport supported_hooks
func supportedHooks() uint32 {
	return uint32(pb.Hook_HOOK_BEFORE_REQUEST.Bit())
}

func bytesAt(ptr, size uint32) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), int(size))
}

func write(b []byte) (uint32, uint32) {
	p := allocExport(uint32(len(b)))
	copy(bytesAt(p, uint32(len(b))), b)
	return p, uint32(len(b))
}

func call(cmd string, args []byte) {
	cp, cl := write([]byte(cmd))
	ap, al := write(args)
	_ = hostCall(cp, cl, ap, al)
}

//go:wasmexport run_hook
func runHook(ptr, size uint32) uint64 {
	blockArgs, _ := proto.Marshal(&pb.BlockRequestArgs{
		Status: 422, Code: "malformed_guest", Message: "refused before returning garbage",
	})
	call("env.block_request", blockArgs)

	respondArgs, _ := proto.Marshal(&pb.RespondRequestArgs{Content: "must be discarded"})
	call("env.respond_request", respondArgs)

	// Not a HookResult: a length-delimited field whose payload is truncated.
	garbage := []byte{0x0a, 0x7f, 0x01, 0x02}
	p, l := write(garbage)
	return uint64(p)<<32 | uint64(l)
}
