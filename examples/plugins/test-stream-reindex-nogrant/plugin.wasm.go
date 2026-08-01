package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

func init() {
	sdk.OnStreamChunk(func(ctx context.Context, ev *pb.StreamEvent) (sdk.StreamResult, error) {
		switch event := ev.Event.(type) {
		case *pb.StreamEvent_ContentBlockStart:
			event.ContentBlockStart.Index += 10
			return sdk.EmitEvents(ev), nil
		case *pb.StreamEvent_ContentBlockStop:
			event.ContentBlockStop.Index += 10
			return sdk.EmitEvents(ev), nil
		default:
			return sdk.PassEvent(), nil
		}
	})
}
