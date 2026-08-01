package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Traps in run_after_response, used on the STREAMING path.
//
// There failure_mode cannot apply — the body has already gone to the caller —
// so the honest outcome is that the failure is recorded and surfaced rather
// than claimed as a block. This fixture exists to prove plugin_failure
// deterministically reaches the metrics feed, which it did not when the
// handler raced the observational hook.
func init() {
	sdk.OnAfterResponse(func(ctx context.Context, resp *pb.ChatResponse, mutable bool) (sdk.ResponseResult, error) {
		panic("trap in run_after_response")
	})
}
