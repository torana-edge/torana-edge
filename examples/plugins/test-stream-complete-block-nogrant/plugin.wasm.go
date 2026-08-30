package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

func completeTextBlock(index int32, text string) []*pb.StreamEvent {
	return []*pb.StreamEvent{
		{Event: &pb.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pb.ContentBlockStart{Index: index, Block: &pb.ContentBlockStart_Text{Text: &pb.TextBlock{}}},
		}},
		{Event: &pb.StreamEvent_TextDelta{TextDelta: text}},
		{Event: &pb.StreamEvent_ContentBlockStop{ContentBlockStop: &pb.ContentBlockStop{Index: index}}},
	}
}

func init() {
	sdk.OnStreamChunk(func(ctx context.Context, ev *pb.StreamEvent) (sdk.StreamResult, error) {
		text, ok := ev.Event.(*pb.StreamEvent_TextDelta)
		if !ok {
			return sdk.PassEvent(), nil
		}
		out := completeTextBlock(7, "invented")
		if text.TextDelta == "two" {
			out = append(out, completeTextBlock(8, "invented again")...)
		}
		return sdk.EmitEvents(out...), nil
	})
}
