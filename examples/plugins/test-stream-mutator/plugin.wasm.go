package main

import (
	"context"

	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

func init() {
	sdk.OnStreamChunk(func(ctx context.Context, ev *pb.StreamEvent) (sdk.StreamResult, error) {
		if textDelta, ok := ev.Event.(*pb.StreamEvent_TextDelta); ok {
			if strings.Contains(textDelta.TextDelta, "secret") {
				textDelta.TextDelta = strings.ReplaceAll(textDelta.TextDelta, "secret", "[REDACTED]")
				return sdk.EmitEvents(ev), nil
			}
		}
		return sdk.PassEvent(), nil
	})
}
