package main

import (
	"context"

	"strings"

	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

func init() {
	sdk.OnStreamChunk(func(ctx context.Context, ev *pb.StreamEvent) (*pb.StreamEventResult, error) {
		if textDelta, ok := ev.Event.(*pb.StreamEvent_TextDelta); ok {
			if strings.Contains(textDelta.TextDelta, "secret") {
				textDelta.TextDelta = strings.ReplaceAll(textDelta.TextDelta, "secret", "[REDACTED]")
				return sdk.Replace(ev), nil
			}
		}
		return sdk.Pass(), nil
	})
}
