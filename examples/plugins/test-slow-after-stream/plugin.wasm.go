package main

import (
	"context"

	"github.com/torana-edge/torana-edge/examples/plugins/blockutil"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// A host-controlled latch for the observational streaming after-response hook.
//
// The hook issues a GRANTED torana_send_request to a test-configured provider
// (see TestObservationalStreamingHookIsOnTheClientCriticalPath). That
// provider's httptest handler blocks until the test releases it, so the hook's
// wall time — and therefore the client's time to EOF, since Go writes the
// chunked terminator when the handler returns — is controlled by the TEST, not
// by CPU speed or an iteration count.
//
// The egress call must have a budget in the test's provider config and the
// env.host_call.torana_send_request permission in plugin.json.
func init() {
	sdk.OnAfterResponse(func(ctx context.Context, resp *pb.ChatResponse, mutable bool) (sdk.ResponseResult, error) {
		_, _ = sdk.SendRequest(
			&pb.ChatRequest{Model: "gpt-x", Messages: []*pb.Message{{Role: "user", Blocks: blockutil.TextBlocks("hi")}}},
			sdk.SendRequestOptions{Provider: "latch", Path: "/release"},
		)
		return sdk.PassResponse(), nil
	})
}
