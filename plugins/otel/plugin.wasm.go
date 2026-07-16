package main

import (
	"context"

	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

// otel emits request-shape metrics the plugin can see, labeled by model. Core
// ops metrics that only the host can observe reliably — request latency,
// upstream status, error rate, and cumulative savings — are emitted host-side
// (see internal/metrics), so this plugin deliberately does not attempt them.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		labels := map[string]string{"model": req.Model}
		sdk.EmitMetric("torana_plugin_requests_total", sdk.MetricCounter, 1, labels)
		sdk.EmitMetric("torana_plugin_request_messages", sdk.MetricHistogram, float64(len(req.Messages)), labels)
		sdk.EmitMetric("torana_plugin_request_tools", sdk.MetricHistogram, float64(len(req.Tools)), labels)
		return nil, nil
	})

	sdk.OnAfterResponse(func(ctx context.Context, resp *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.EmitMetric("torana_plugin_responses_total", sdk.MetricCounter, 1, map[string]string{"model": resp.Model})
		return nil, nil
	})
}
