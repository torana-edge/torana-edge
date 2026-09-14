package main

import (
	"context"
	"errors"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}
func init() {
	sdk.OnStreamChunk(func(ctx context.Context, ev *pb.StreamEvent) (sdk.StreamResult, error) {
		if streamErr := ev.GetError(); streamErr != nil && streamErr.Code == 499 {
			// Test-only malformed return: a provider error is a terminal scope
			// boundary, so this unterminated invented block must be caught before
			// any deferred journal output is released.
			return sdk.EmitEvents(&pb.StreamEvent{Event: &pb.StreamEvent_ContentBlockStart{
				ContentBlockStart: &pb.ContentBlockStart{Index: 9, Block: &pb.ContentBlockStart_ToolCall{
					ToolCall: &pb.ToolCallRef{Id: "invented", Name: "run"},
				}},
			}}), nil
		}
		if ev.GetContentBlockStart().GetToolCall() != nil {
			return sdk.SuppressEvent(), nil
		}
		if d := ev.GetToolCallDelta(); d != nil {
			if d.ArgumentsDelta == "FAIL" {
				return sdk.PassEvent(), errors.New("intentional deferred callback failure")
			}
			return sdk.SuppressEvent(), nil
		}
		if ev.GetContentBlockStop() != nil {
			return sdk.SuppressEvent(), nil
		}
		if ev.GetTextDelta() != "" {
			return sdk.EmitEvents(&pb.StreamEvent{Event: &pb.StreamEvent_TextDelta{TextDelta: "SHOULD-NOT-RUN"}}), nil
		}
		return sdk.PassEvent(), nil
	})
}
