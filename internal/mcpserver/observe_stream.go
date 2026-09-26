package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
)

const observedArgumentLimit = 64 << 10
const observedCallLimit = 64

type observedCall struct {
	id, tool  string
	arguments strings.Builder
	complete  bool
}

// ObserveStream is a transparent event tee. Evidence is committed only after
// a clean, completed stream closes; errors, cancellation and unfinished tool
// calls never grant bindings. The caller passes the client-facing event stream.
func (c *Correlator) ObserveStream(ctx context.Context, input <-chan engine.StreamEvent, shape, conversation string, servers []string) <-chan engine.StreamEvent {
	output := make(chan engine.StreamEvent)
	servers = append([]string(nil), servers...)
	go func() {
		defer close(output)
		calls := map[int]*observedCall{}
		responseID, finish := "", ""
		failed, overflow := false, false
		argumentBytes := 0
		for {
			var event engine.StreamEvent
			var open bool
			select {
			case <-ctx.Done():
				return
			case event, open = <-input:
			}
			if !open {
				break
			}
			if event.MessageStart != nil {
				responseID = event.MessageStart.ID
			}
			if event.Error != nil {
				failed = true
			}
			if event.FinishReason != "" {
				finish = event.FinishReason
			}
			if start := event.ToolCallStart; start != nil {
				if tool := responseTool(start.Name, servers); tool != "" {
					if start.Index < 0 {
						failed = true
					}
					if _, exists := calls[start.Index]; exists {
						failed = true
					}
					if len(calls) >= observedCallLimit {
						overflow = true
					} else {
						calls[start.Index] = &observedCall{id: start.ID, tool: tool}
					}
				}
			}
			if delta := event.ToolCallDelta; delta != nil {
				if call := calls[delta.Index]; call != nil {
					if call.complete || delta.InputTextDelta != nil {
						failed = true
					}
					if len(delta.ArgumentsDelta) > observedArgumentLimit-argumentBytes {
						overflow = true
					} else if !overflow {
						argumentBytes += len(delta.ArgumentsDelta)
						call.arguments.WriteString(delta.ArgumentsDelta)
					}
				}
			}
			if end := event.ToolCallEnd; end != nil {
				if call := calls[end.Index]; call != nil {
					call.complete = true
				}
			}
			select {
			case <-ctx.Done():
				return
			case output <- event:
			}
		}
		if ctx.Err() != nil || failed {
			return
		}
		now := time.Now()
		if overflow {
			// Dropping evidence could hide an ambiguity with another stream.
			c.mu.Lock()
			c.saturatedUntil = now.Add(correlationTTL)
			c.mu.Unlock()
			return
		}
		switch finish {
		case "stop", "tool_calls", "tool_use", "end_turn", "STOP":
		default:
			return
		}
		switch shape {
		case "anthropic", "openai-chat", "openai-responses", "gemini", "gemini-codeassist":
		default:
			return
		}
		for _, call := range calls {
			if !call.complete {
				return
			}
		}
		for index, call := range calls {
			id := call.id
			if id == "" && strings.HasPrefix(shape, "gemini") && responseID != "" {
				id = fmt.Sprintf("gemini:%s:%d", responseID, index)
			}
			c.Record(call.tool, []byte(call.arguments.String()), Binding{ConversationID: conversation, CallID: id}, now)
		}
	}()
	return output
}
