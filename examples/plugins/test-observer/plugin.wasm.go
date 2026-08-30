package main

import (
	"context"
	"fmt"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

// Test fixture exercising the response signal end-to-end:
//   - On a normal response it rewrites the assistant content to
//     "observed status=<s> in=<i> out=<o>" so the e2e can assert that
//     run_after_response received latency/status/usage.
//   - On the error-path observe-only invocation (no message) it caches the
//     observed status; the next request's model name is tagged with it, which
//     an echoing test upstream then reveals.
//
// v1 delivered all of this as a "_response" blob inside ToranaMetaJson, because
// run_after_response was handed a ChatRequest and there was nowhere else to put
// it. current ABI has a real ChatResponse, so status, usage and duration are ordinary
// typed fields and the fixture reads them directly. Nothing here has to know
// the shape of a side-channel JSON object any more.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		v, herr, err := sdk.CacheGet("observed_error_status")
		if err != nil {
			return sdk.PassRequest(), err
		}
		// A miss is ordinary: the error path may not have run yet.
		if sdk.IsNotFound(herr) {
			return sdk.PassRequest(), nil
		}
		if herr != nil {
			return sdk.PassRequest(), fmt.Errorf("cache_get refused: %s", herr.Message)
		}
		if v == "" {
			return sdk.PassRequest(), nil
		}
		req.Model = req.Model + "+err" + v
		return sdk.ReplaceRequest(req), nil
	})

	sdk.OnAfterResponse(func(ctx context.Context, resp *pb.ChatResponse, mutable bool) (sdk.ResponseResult, error) {
		// No assistant message means the upstream-error or streamed shape:
		// observe only, there is nothing to rewrite. v1 could not express this
		// — it received the outbound request and saw its message history.
		if resp.Message == nil {
			if herr, err := sdk.CacheSet("observed_error_status",
				fmt.Sprintf("%d", resp.UpstreamStatus)); err != nil {
				return sdk.PassResponse(), err
			} else if herr != nil {
				return sdk.PassResponse(), fmt.Errorf("cache_set refused: %s", herr.Message)
			}
			return sdk.PassResponse(), nil
		}

		// mutable is false on paths whose bytes already went to the caller.
		// Returning a replacement there would be discarded, so say nothing.
		if !mutable {
			return sdk.PassResponse(), nil
		}

		var in, out int32
		if resp.Usage != nil {
			in, out = resp.Usage.InputTokens, resp.Usage.OutputTokens
		}
		content := fmt.Sprintf("observed status=%d in=%d out=%d",
			resp.UpstreamStatus, in, out)
		resp.Message.Content = &content
		return sdk.ReplaceResponse(resp), nil
	})
}
