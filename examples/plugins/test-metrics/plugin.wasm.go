package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/pb"
)

func main() {}

// Test fixture for the env.emit_metric ABI.
//
// It emits one of each metric type with a label set, so a test can assert the
// host received the name, type, value and labels intact — the whole boundary,
// rather than "a metric arrived". A real observability plugin's values depend on
// request timing, which makes it a poor thing to assert against.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		labels := map[string]string{"model": req.Model, "fixture": "test-metrics"}
		sdk.EmitMetric("test_counter", sdk.MetricCounter, 1, labels)
		sdk.EmitMetric("test_histogram", sdk.MetricHistogram, 42.5, labels)
		sdk.EmitMetric("test_gauge", sdk.MetricGauge, 7, labels)
		// Deliberately no mutation: emitting a metric must not alter the
		// request, and a test asserting the prefix is untouched needs that.
		return nil, nil
	})
}
