package main

import (
	"strings"

	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

func init() {
	sdk.OnStreamChunk(func(ev *pb.StreamEvent) (*pb.StreamEvent, error) {
		if textDelta, ok := ev.Event.(*pb.StreamEvent_TextDelta); ok {
			if strings.Contains(textDelta.TextDelta, "secret") {
				textDelta.TextDelta = strings.ReplaceAll(textDelta.TextDelta, "secret", "[REDACTED]")
				return ev, nil
			}
		}
		return nil, nil
	})
}
