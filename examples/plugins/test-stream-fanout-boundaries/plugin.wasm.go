package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// One host TextDelta becomes two complete text-block boundaries. The following
// fixture reindexes them without a topology grant, proving downstream scopes
// come from each plugin's accepted stream rather than only the host event.
func init() {
	sdk.OnStreamChunk(func(ctx context.Context, ev *pb.StreamEvent) (sdk.StreamResult, error) {
		if _, ok := ev.Event.(*pb.StreamEvent_TextDelta); !ok {
			return sdk.PassEvent(), nil
		}
		start := func(index int32) *pb.StreamEvent {
			return &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{
				ContentBlockStart: &pb.ContentBlockStart{Index: index, Block: &pb.ContentBlockStart_Text{Text: &pb.TextBlock{}}},
			}}
		}
		stop := func(index int32) *pb.StreamEvent {
			return &pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStop{
				ContentBlockStop: &pb.ContentBlockStop{Index: index},
			}}
		}
		return sdk.EmitEvents(start(0), stop(0), start(1), stop(1)), nil
	})
}
