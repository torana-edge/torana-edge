package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

// Traps in run_after_response under failure_mode "block".
//
// The non-streaming response is the one response path where the body has NOT
// been written yet — ModifyResponse still owns it — so failure_mode can and
// must be applied. Forwarding the provider's body after a plugin refused it is
// the same fail-open as sending the request upstream after a refusal.
func init() {
	sdk.OnAfterResponse(func(ctx context.Context, resp *pb.ChatResponse, mutable bool) (sdk.ResponseResult, error) {
		panic("trap in run_after_response")
	})
}
