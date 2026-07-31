package main

import (
	"context"
	"fmt"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Test fixture for env.original_request / env.original_response: it mutates
// the request model on the way in, then on the way out proves it can still
// see (a) the caller's pristine model via OriginalRequest and (b) the raw
// upstream body via OriginalResponse — by writing markers into the assistant
// content.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		req.Model = req.Model + "-mutated"
		return sdk.ReplaceRequest(req), nil
	})

	sdk.OnAfterResponse(func(ctx context.Context, resp *pb.ChatResponse, mutable bool) (sdk.ResponseResult, error) {
		if resp.Message == nil {
			return sdk.PassResponse(), nil
		}
		origModel := "unavailable"
		if orig, ok := sdk.OriginalRequest(); ok {
			origModel = orig.Model
		}
		rawMarker := "raw=missing"
		if raw, ok := sdk.OriginalResponse(); ok && strings.Contains(string(raw), "pristine-upstream-marker") {
			rawMarker = "raw=pristine"
		}
		resp.Message.Content = fmt.Sprintf("orig-model=%s %s", origModel, rawMarker)
		return sdk.ReplaceResponse(resp), nil
	})
}
