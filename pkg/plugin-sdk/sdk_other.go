//go:build !wasip1

package plugin_sdk

import (
	"context"
	"github.com/torana-edge/torana-edge/pkg/pb"
)

//nolint:unused
func alloc(size uint32) uint32 { return 0 }

//nolint:unused
func dealloc(ptr uint32, size uint32)   {}
func ReadBytes(ptr, size uint32) []byte { return nil }
func WriteResult(data []byte) uint64    { return 0 }

func OnBeforeRequest(handler func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error)) {
}
func OnAfterResponse(handler func(ctx context.Context, resp *pb.ChatRequest) (*pb.ChatRequest, error)) {
}
func OnStreamChunk(handler func(ctx context.Context, chunk *pb.StreamEvent) (*pb.StreamEventResult, error)) {
}

func Pass() *pb.StreamEventResult     { return nil }
func Suppress() *pb.StreamEventResult { return &pb.StreamEventResult{Handled: true} }
func Replace(ev *pb.StreamEvent) *pb.StreamEventResult {
	return &pb.StreamEventResult{Handled: true, Events: []*pb.StreamEvent{ev}}
}
func Emit(evs ...*pb.StreamEvent) *pb.StreamEventResult {
	return &pb.StreamEventResult{Handled: true, Events: evs}
}

const (
	LogLevelDebug = 0
	LogLevelInfo  = 1
)

const (
	MetricCounter   = 0
	MetricHistogram = 1
)

func Log(msg string, level int32) {}
func EmitMetric(name string, metricType int32, value float64, labels map[string]string) {
}

func HostCall(cmd string, args string) (string, error) { return "", nil }

func PluginConfig() string { return "{}" }
