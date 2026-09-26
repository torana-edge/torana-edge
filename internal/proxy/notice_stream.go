package proxy

import (
	"context"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// appendNoticeEvents holds the terminal event until the source closes. A
// provider error or truncated stream cannot receive a notice or a clean
// completion marker. The inserted text block is serialized in the client's
// wire shape by the existing native/bridge serializer.
func appendNoticeEvents(ctx context.Context, input <-chan engine.StreamEvent, notice string) <-chan engine.StreamEvent {
	if notice == "" {
		return input
	}
	output := make(chan engine.StreamEvent)
	go func() {
		defer close(output)
		var finish engine.StreamEvent
		maxIndex := -1
		hadTool, failed := false, false
		send := func(event engine.StreamEvent) bool {
			select {
			case output <- event:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for event := range input {
			if event.BlockStart != nil && event.BlockStart.Index > maxIndex {
				maxIndex = event.BlockStart.Index
			}
			if event.ToolCallStart != nil {
				hadTool = true
				if event.ToolCallStart.Index > maxIndex {
					maxIndex = event.ToolCallStart.Index
				}
			}
			if event.Error != nil {
				failed = true
			}
			if event.FinishReason != "" {
				finish = event
				continue
			}
			if !send(event) {
				for range input {
				}
				return
			}
		}
		if ctx.Err() != nil || failed {
			return
		}
		if finish.FinishReason == "stop" && !hadTool {
			index := maxIndex + 1
			if !send(engine.StreamEvent{BlockStart: &engine.BlockStart{Index: index, Kind: engine.BlockKindText}}) ||
				!send(engine.StreamEvent{TextDelta: &notice}) ||
				!send(engine.StreamEvent{BlockStop: &engine.BlockStop{Index: index}}) {
				return
			}
		}
		if finish.FinishReason != "" {
			_ = send(finish)
		}
	}()
	return output
}
