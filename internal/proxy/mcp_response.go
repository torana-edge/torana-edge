package proxy

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// Keep the terminal suffix in order but do not expose its finish marker before
// the observer closes. ObserveStream records evidence before closing its output.
// The bound avoids buffering an unbounded malformed post-completion stream.
func commitMCPStreamBeforeFinish(ctx context.Context, input <-chan engine.StreamEvent, commit func()) <-chan engine.StreamEvent {
	out := make(chan engine.StreamEvent)
	go func() {
		defer close(out)
		var suffix []engine.StreamEvent
		send := func(event engine.StreamEvent) bool {
			select {
			case out <- event:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case event, open := <-input:
				if !open {
					if ctx.Err() != nil {
						return
					}
					commit()
					for _, pending := range suffix {
						if !send(pending) {
							return
						}
					}
					return
				}
				if event.FinishReason != "" || len(suffix) != 0 {
					if len(suffix) >= 64 {
						send(engine.StreamEvent{Error: &engine.StreamError{Code: 502, Message: "too many events after stream completion"}})
						return
					}
					suffix = append(suffix, event)
				} else if !send(event) {
					return
				}
			}
		}
	}()
	return out
}

func (s *Server) observeMCPResponse(body []byte, shape string, rs *reqState, status int) {
	if rs == nil || rs.ConversationID == "" || s.mcpCorrelation == nil || status < 200 || status >= 300 || rs.UpstreamStatus < 200 || rs.UpstreamStatus >= 300 {
		return
	}
	cfg := s.GetConfig().Providers.MCP
	if !cfg.Enabled {
		return
	}
	format := shape
	if shape == "openai-chat" || shape == "openai-responses" {
		format = "openai"
	}
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) != nil {
		return
	}
	refs := extractResponse(format, parsed, body)
	response := &engine.ChatResponse{ID: refs.id, Message: refs.assistantMessage(), FinishReason: refs.finishReason, UpstreamStatus: status}
	s.mcpCorrelation.ObserveResponse(response, shape, rs.ConversationID, strconv.FormatUint(rs.ID, 10), cfg.ResponseServerNames(), time.Now())
}
