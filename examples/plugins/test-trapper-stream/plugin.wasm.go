package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Traps on the first stream chunk under failure_mode "block".
//
// The headers and some body have already gone to the caller, so nothing can be
// refused any more. Replaying the event was the fail-open: the plugin refused
// that content and forwarding it delivers exactly what the block policy exists
// to withhold. Terminating leaves a visibly truncated stream instead.
func init() {
	sdk.OnStreamChunk(func(ctx context.Context, ev *pb.StreamEvent) (sdk.StreamResult, error) {
		if _, ok := ev.Event.(*pb.StreamEvent_TextDelta); ok {
			panic("trap on a text delta")
		}
		return sdk.PassEvent(), nil
	})
}
