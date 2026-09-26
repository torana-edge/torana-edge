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
	id, tool, name string
	arguments      strings.Builder
	complete       bool
}

// ObserveStream is a transparent event tee. Evidence is committed only after
// a clean, completed stream closes; errors, cancellation and unfinished tool
// calls never grant bindings. The caller passes the client-facing event stream.
// stopSource must unblock the producer and cause input to close on cancellation
// (usually by closing the upstream HTTP body). It may be nil only when input is
// already closed or its producer independently observes this same context.
func (c *Correlator) ObserveStream(ctx context.Context, input <-chan engine.StreamEvent, shape, conversation, requestID string, servers []string, stopSource func()) <-chan engine.StreamEvent {
	output := make(chan engine.StreamEvent)
	servers = append([]string(nil), servers...)
	go func() {
		defer close(output)
		calls := map[int]*observedCall{}
		finish := ""
		failed := false
		overflow := map[string]bool{}
		cancelInput := func() {
			// The caller must close the upstream reader (or stop its producer)
			// here. Parsers use unbuffered sends without context selects: drain
			// their real input after closing the reader to release in-flight sends.
			if stopSource != nil {
				stopSource()
			}
			go func() {
				for range input {
				}
			}()
		}
		for {
			var event engine.StreamEvent
			var open bool
			select {
			case <-ctx.Done():
				cancelInput()
				return
			case event, open = <-input:
			}
			if !open {
				break
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
						overflow[tool] = true
					} else {
						calls[start.Index] = &observedCall{id: start.ID, tool: tool, name: start.Name}
					}
				}
			}
			if delta := event.ToolCallDelta; delta != nil {
				if call := calls[delta.Index]; call != nil {
					if call.complete || delta.InputTextDelta != nil {
						failed = true
					}
					if len(delta.ArgumentsDelta) > observedArgumentLimit-call.arguments.Len() {
						overflow[call.tool] = true
					} else if !overflow[call.tool] {
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
				cancelInput()
				return
			case output <- event:
			}
		}
		if ctx.Err() != nil || failed {
			return
		}
		now := time.Now()
		if len(overflow) > 0 {
			// Scope saturation to affected tools, not all MCP discovery/calls.
			// Even oversized raw arguments can normalize to a small request
			// (e.g. whitespace); simply dropping them could hide an ambiguity.
			c.mu.Lock()
			if c.saturatedTools == nil {
				c.saturatedTools = map[string]time.Time{}
			}
			for tool := range overflow {
				c.saturatedTools[tool] = now.Add(correlationTTL)
			}
			c.mu.Unlock()
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
			if overflow[call.tool] {
				continue
			}
			id := call.id
			if strings.HasPrefix(shape, "gemini") && (id == "" || id == call.name) {
				id = ""
				if requestID != "" {
					id = fmt.Sprintf("gemini:%s:%d", requestID, index)
				}
			}
			c.Record(call.tool, []byte(call.arguments.String()), Binding{ConversationID: conversation, CallID: id}, now)
		}
	}()
	return output
}
