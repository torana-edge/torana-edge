package main

import (
	"context"

	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.EmitMetric("torana_requests_total", sdk.MetricCounter, 1)
		sdk.EmitMetric("torana_request_messages", sdk.MetricHistogram, float64(len(req.Messages)))
		return nil, nil
	})

	sdk.OnAfterResponse(func(ctx context.Context, resp *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.EmitMetric("torana_responses_total", sdk.MetricCounter, 1)
		return nil, nil
	})
}
