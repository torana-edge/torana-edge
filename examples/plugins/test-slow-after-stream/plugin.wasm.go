package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Burns measurable wall time in the observational streaming after-response
// hook, so a test can pin whether that time lands on the client's critical
// path.
//
// It does, deliberately: Go's HTTP server writes the chunked terminator when
// the handler returns, and the handler waits for this hook. The test exists so
// that behaviour is a recorded decision rather than a surprise — the earlier
// comment claimed the client already had EOF, which was wrong.
const spinIterations = 60_000_000

func init() {
	sdk.OnAfterResponse(func(ctx context.Context, resp *pb.ChatResponse, mutable bool) (sdk.ResponseResult, error) {
		sink := 0
		for i := 0; i < spinIterations; i++ {
			sink += i & 7
		}
		if sink < 0 {
			return sdk.PassResponse(), nil
		}
		return sdk.PassResponse(), nil
	})
}
