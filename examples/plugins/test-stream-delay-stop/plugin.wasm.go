package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnStreamChunk(func(ctx context.Context, ev *pb.StreamEvent) (sdk.StreamResult, error) {
		switch ev.Event.(type) {
		case *pb.StreamEvent_ContentBlockStop:
			return sdk.SuppressEvent(), nil
		case *pb.StreamEvent_Usage:
			return sdk.EmitEvents(
				&pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStop{ContentBlockStop: &pb.ContentBlockStop{Index: 0}}},
				ev,
			), nil
		default:
			return sdk.PassEvent(), nil
		}
	})
}
