package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

func init() {
	sdk.OnStreamChunk(func(ctx context.Context, ev *pb.StreamEvent) (sdk.StreamResult, error) {
		switch event := ev.Event.(type) {
		case *pb.StreamEvent_ContentBlockStart:
			return sdk.EmitEvents(
				ev,
				&pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStop{ContentBlockStop: &pb.ContentBlockStop{Index: event.ContentBlockStart.Index}}},
			), nil
		case *pb.StreamEvent_ContentBlockStop:
			return sdk.SuppressEvent(), nil
		default:
			return sdk.PassEvent(), nil
		}
	})
}
