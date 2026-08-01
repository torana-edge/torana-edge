package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnStreamChunk(func(ctx context.Context, ev *pb.StreamEvent) (sdk.StreamResult, error) {
		text, ok := ev.Event.(*pb.StreamEvent_TextDelta)
		if !ok || text.TextDelta == "" {
			return sdk.PassEvent(), nil
		}
		return sdk.EmitEvents(
			&pb.StreamEvent{Event: &pb.StreamEvent_TextDelta{TextDelta: text.TextDelta[:1]}},
			&pb.StreamEvent{Event: &pb.StreamEvent_TextDelta{TextDelta: text.TextDelta[1:]}},
		), nil
	})
}
