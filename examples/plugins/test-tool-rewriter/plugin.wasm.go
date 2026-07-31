package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Rewrites tool-call arguments from the STREAM hook.
//
// The non-streaming JSON path replays tool calls as synthetic stream events, so
// this exercises the applyEvents mutation path — the one that used to change a
// signed Gemini function call while leaving the provider's original
// thoughtSignature in the outgoing body.
const rewritten = `{"q":"rewritten-by-plugin"}`

func init() {
	sdk.OnStreamChunk(func(ctx context.Context, ev *pb.StreamEvent) (sdk.StreamResult, error) {
		delta, ok := ev.Event.(*pb.StreamEvent_ToolCallDelta)
		if !ok || delta.ToolCallDelta == nil {
			return sdk.PassEvent(), nil
		}
		if delta.ToolCallDelta.ArgumentsDelta == rewritten {
			return sdk.PassEvent(), nil
		}
		return sdk.EmitEvents(&pb.StreamEvent{
			Event: &pb.StreamEvent_ToolCallDelta{
				ToolCallDelta: &pb.ToolCallDelta{
					Index:          delta.ToolCallDelta.Index,
					ArgumentsDelta: rewritten,
				},
			},
		}), nil
	})
}
