package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

// otel emits request-shape metrics on the way in and, on the way out, the
// per-request signals the host exposes via ToranaMeta._response: latency,
// upstream status class, and provider-reported token usage. Core ops metrics
// the host can observe more reliably (every response, including vetoes) are
// also emitted host-side (see internal/metrics); the plugin-side series exist
// so operators can slice by whatever labels plugins add.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		labels := map[string]string{"model": req.Model}
		sdk.EmitMetric("torana_plugin_requests_total", sdk.MetricCounter, 1, labels)
		sdk.EmitMetric("torana_plugin_request_messages", sdk.MetricHistogram, float64(len(req.Messages)), labels)
		sdk.EmitMetric("torana_plugin_request_tools", sdk.MetricHistogram, float64(len(req.Tools)), labels)
		return nil, nil
	})

	sdk.OnAfterResponse(func(ctx context.Context, resp *pb.ChatRequest) (*pb.ChatRequest, error) {
		labels := map[string]string{"model": resp.Model}

		var meta struct {
			Response *struct {
				DurationMs     float64 `json:"duration_ms"`
				UpstreamStatus int     `json:"upstream_status"`
				Usage          struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"_response"`
		}
		if len(resp.ToranaMetaJson) > 0 {
			_ = json.Unmarshal(resp.ToranaMetaJson, &meta)
		}
		if meta.Response == nil {
			sdk.EmitMetric("torana_plugin_responses_total", sdk.MetricCounter, 1, labels)
			return nil, nil
		}

		r := meta.Response
		labels["status_class"] = statusClass(r.UpstreamStatus)
		sdk.EmitMetric("torana_plugin_responses_total", sdk.MetricCounter, 1, labels)
		sdk.EmitMetric("torana_plugin_request_duration_ms", sdk.MetricHistogram, r.DurationMs, map[string]string{"model": resp.Model})
		if r.Usage.InputTokens > 0 {
			sdk.EmitMetric("torana_plugin_tokens", sdk.MetricCounter, float64(r.Usage.InputTokens), map[string]string{"model": resp.Model, "direction": "input"})
		}
		if r.Usage.OutputTokens > 0 {
			sdk.EmitMetric("torana_plugin_tokens", sdk.MetricCounter, float64(r.Usage.OutputTokens), map[string]string{"model": resp.Model, "direction": "output"})
		}
		return nil, nil
	})
}

func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	default:
		return fmt.Sprintf("%d", status)
	}
}
