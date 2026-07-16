package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

// Test fixture exercising the host's _response signal end-to-end:
//   - On a normal response it rewrites the assistant content to
//     "observed status=<s> in=<i> out=<o>" so the e2e can assert that
//     run_after_response received latency/status/usage.
//   - On the error-path observe-only invocation (no messages) it caches the
//     observed status; the next request's model name is tagged with it, which
//     an echoing test upstream then reveals.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		if v, err := sdk.HostCall("env.cache_get", "observed_error_status"); err == nil && v != "" {
			req.Model = req.Model + "+err" + v
			return req, nil
		}
		return nil, nil
	})

	sdk.OnAfterResponse(func(ctx context.Context, resp *pb.ChatRequest) (*pb.ChatRequest, error) {
		var meta struct {
			Response *struct {
				DurationMs     float64 `json:"duration_ms"`
				UpstreamStatus int     `json:"upstream_status"`
				Usage          struct {
					In  int `json:"input_tokens"`
					Out int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"_response"`
		}
		if len(resp.ToranaMetaJson) > 0 {
			_ = json.Unmarshal(resp.ToranaMetaJson, &meta)
		}
		if meta.Response == nil {
			return nil, nil
		}
		r := meta.Response

		if len(resp.Messages) == 0 {
			// Error-path shape: observe-only, nothing to mutate.
			args, _ := json.Marshal(map[string]any{
				"key":   "observed_error_status",
				"value": fmt.Sprintf("%d", r.UpstreamStatus),
			})
			_, _ = sdk.HostCall("env.cache_set", string(args))
			return nil, nil
		}

		resp.Messages[0].Content = fmt.Sprintf("observed status=%d in=%d out=%d",
			r.UpstreamStatus, r.Usage.In, r.Usage.Out)
		return resp, nil
	})
}
