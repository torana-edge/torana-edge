//go:build wasip1

package plugin_sdk

import (
	"context"
	"sync"
	"unsafe"

	"github.com/torana-edge/torana-edge/pkg/pb"
	"google.golang.org/protobuf/proto"
)

var (
	pinned   = make(map[uint32][]byte)
	pinMutex sync.Mutex
)

//go:wasmexport alloc
func alloc(size uint32) uint32 {
	if size == 0 {
		return 0
	}
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

var chatRequestHandler func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error)

// OnBeforeRequest registers the handler for chat requests.
func OnBeforeRequest(handler func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error)) {
	chatRequestHandler = handler
}

//go:wasmexport run_before_request
func run_before_request(reqID uint64, ptr, size uint32) uint64 {
	if chatRequestHandler == nil {
		return 0
	}
	inputBytes := ReadBytes(ptr, size)
	var req pb.ChatRequest
	if err := proto.Unmarshal(inputBytes, &req); err != nil {
		Log("on_chat_request unmarshal err: "+err.Error(), LogLevelInfo)
		return 0
	}

	out, err := chatRequestHandler(context.WithValue(context.Background(), "reqID", reqID), &req)
	if err != nil || out == nil {
		return 0
	}

	outBytes, err := proto.Marshal(out)
	if err != nil || len(outBytes) == 0 {
		return 0
	}
	return WriteResult(outBytes)
}

var chatResponseHandler func(ctx context.Context, resp *pb.ChatRequest) (*pb.ChatRequest, error)

// OnAfterResponse registers the handler for chat responses.
func OnAfterResponse(handler func(ctx context.Context, resp *pb.ChatRequest) (*pb.ChatRequest, error)) {
	chatResponseHandler = handler
}

//go:wasmexport run_after_response
func run_after_response(reqID uint64, ptr, size uint32) uint64 {
	if chatResponseHandler == nil {
		return 0
	}
	inputBytes := ReadBytes(ptr, size)
	var resp pb.ChatRequest
	if err := proto.Unmarshal(inputBytes, &resp); err != nil {
		return 0
	}

	out, err := chatResponseHandler(context.WithValue(context.Background(), "reqID", reqID), &resp)
	if err != nil || out == nil {
		return 0
	}

	outBytes, err := proto.Marshal(out)
	if err != nil || len(outBytes) == 0 {
		return 0
	}
	return WriteResult(outBytes)
}

var streamChunkHandler func(ctx context.Context, chunk *pb.StreamEvent) (*pb.StreamEvent, error)

// OnStreamChunk registers the handler for stream chunks.
func OnStreamChunk(handler func(ctx context.Context, chunk *pb.StreamEvent) (*pb.StreamEvent, error)) {
	streamChunkHandler = handler
}

//go:wasmexport run_on_stream_chunk
func run_on_stream_chunk(reqID uint64, ptr, size uint32) uint64 {
	if streamChunkHandler == nil {
		return 0
	}
	inputBytes := ReadBytes(ptr, size)
	var chunk pb.StreamEvent
	if err := proto.Unmarshal(inputBytes, &chunk); err != nil {
		return 0
	}

	out, err := streamChunkHandler(context.WithValue(context.Background(), "reqID", reqID), &chunk)
	if err != nil || out == nil {
		return 0
	}

	outBytes, err := proto.Marshal(out)
	if err != nil || len(outBytes) == 0 {
		return 0
	}
	return WriteResult(outBytes)
}

//go:wasmimport env log
func hostLog(level int32, ptr uint32, length uint32)

const (
	LogLevelDebug = 0
	LogLevelInfo  = 1
)

func Log(msg string, level int32) {
	b := []byte(msg)
	if len(b) == 0 {
		return
	}
	ptr := alloc(uint32(len(b)))
	copy(ReadBytes(ptr, uint32(len(b))), b)
	hostLog(level, ptr, uint32(len(b)))
	dealloc(ptr, uint32(len(b)))
}

//go:wasmimport env host_call
func hostCall(cmdPtr uint32, cmdLen uint32, argsPtr uint32, argsLen uint32) uint64

// HostCall invokes a registered host function by name.
func HostCall(cmd string, args string) (string, error) {
	cb := []byte(cmd)
	ab := []byte(args)
	if len(cb) == 0 {
		return "", nil
	}

	cPtr := alloc(uint32(len(cb)))
	copy(ReadBytes(cPtr, uint32(len(cb))), cb)
	defer dealloc(cPtr, uint32(len(cb)))

	var aPtr uint32
	if len(ab) > 0 {
		aPtr = alloc(uint32(len(ab)))
		copy(ReadBytes(aPtr, uint32(len(ab))), ab)
		defer dealloc(aPtr, uint32(len(ab)))
	}

	ret := hostCall(cPtr, uint32(len(cb)), aPtr, uint32(len(ab)))
	if ret == 0 {
		return "", nil
	}

	outPtr := uint32(ret >> 32)
	outLen := uint32(ret & 0xFFFFFFFF)
	res := string(ReadBytes(outPtr, outLen))
	dealloc(outPtr, outLen)

	return res, nil
}
