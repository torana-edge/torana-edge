package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

// Records a block verdict, a respond verdict, and THEN traps.
//
// This is the trap-semantics case: the block must SURVIVE (a security verdict
// fails closed — code that decided to refuse and then crashed still refused),
// while the respond is DISCARDED (a half-built synthetic response from code
// that crashed immediately after is not trustworthy). And the block must
// short-circuit, so no downstream plugin sees a request that has been refused.
//
// failure_mode is "pass" on purpose: without it the pipeline would return early
// for its own reason and the short-circuit would not be what stopped the run.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		sdk.BlockRequest(422, "blocked_then_trapped", "refused before trapping")
		sdk.RespondRequest("this respond must be discarded")
		panic("trap after recording verdicts")
	})
}
