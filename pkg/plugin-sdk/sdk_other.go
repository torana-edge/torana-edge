//go:build !wasip1

package plugin_sdk

import (
	"context"
	"github.com/torana-edge/torana-edge/pkg/pb"
)

func alloc(size uint32) uint32          { return 0 }
func dealloc(ptr uint32, size uint32)   {}
func ReadBytes(ptr, size uint32) []byte { return nil }
func WriteResult(data []byte) uint64    { return 0 }

func OnBeforeRequest(handler func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error)) {
}
func OnAfterResponse(handler func(ctx context.Context, resp *pb.ChatRequest) (*pb.ChatRequest, error)) {
}
func OnStreamChunk(handler func(ctx context.Context, chunk *pb.StreamEvent) (*pb.StreamEvent, error)) {
}

const (
	LogLevelDebug = 0
	LogLevelInfo  = 1
)

func Log(msg string, level int32) {}

func HostCall(cmd string, args string) (string, error) { return "", nil }
