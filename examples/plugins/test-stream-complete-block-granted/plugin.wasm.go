package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

func init() {
	sdk.OnStreamChunk(func(ctx context.Context, ev *pb.StreamEvent) (sdk.StreamResult, error) {
		if _, ok := ev.Event.(*pb.StreamEvent_TextDelta); !ok {
			return sdk.PassEvent(), nil
		}
		return sdk.EmitEvents(
			&pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{
				ContentBlockStart: &pb.ContentBlockStart{Index: 7, Block: &pb.ContentBlockStart_Text{Text: &pb.TextBlock{}}},
			}},
			&pb.StreamEvent{Event: &pb.StreamEvent_TextDelta{TextDelta: "invented"}},
			&pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStop{ContentBlockStop: &pb.ContentBlockStop{Index: 7}}},
		), nil
	})
}
