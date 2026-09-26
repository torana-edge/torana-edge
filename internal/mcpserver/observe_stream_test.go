package mcpserver

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func observationEvents() []engine.StreamEvent {
	return []engine.StreamEvent{
		{MessageStart: &engine.StreamMessageStart{ID: "response"}},
		{ToolCallStart: &engine.ToolCallStart{Index: 3, ID: "call", Name: "mcp__torana__torana_search"}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 3, ArgumentsDelta: `{"query":`}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 3, ArgumentsDelta: `"status"}`}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 3}},
		{FinishReason: "tool_calls"},
	}
}

func TestObserveStreamPreservesEventsAndCommitsCompleteCall(t *testing.T) {
	for _, shape := range []string{"anthropic", "openai-chat", "openai-responses", "gemini", "gemini-codeassist"} {
		t.Run(shape, func(t *testing.T) {
			c := NewCorrelator()
			events := observationEvents()
			if strings.HasPrefix(shape, "gemini") {
				events[1].ToolCallStart.ID = ""
			}
			input := make(chan engine.StreamEvent, len(events))
			for _, event := range events {
				input <- event
			}
			close(input)
			var got []engine.StreamEvent
			for event := range c.ObserveStream(context.Background(), input, shape, "conversation", []string{"torana"}) {
				got = append(got, event)
			}
			if !reflect.DeepEqual(got, events) {
				t.Fatal("events changed")
			}
			binding, ok := c.Consume("torana_search", json.RawMessage(`{"query":"status"}`), time.Now())
			if !ok || binding.ConversationID != "conversation" || binding.CallID == "" {
				t.Fatalf("binding=%+v ok=%t", binding, ok)
			}
		})
	}
}

func TestObserveStreamRejectsFailureTruncationAndOverflow(t *testing.T) {
	for _, scenario := range []string{"error", "truncated", "unfinished", "overflow"} {
		t.Run(scenario, func(t *testing.T) {
			c := NewCorrelator()
			events := observationEvents()
			switch scenario {
			case "error":
				events = append(events, engine.StreamEvent{Error: &engine.StreamError{Message: "failure"}})
			case "truncated":
				events = events[:len(events)-1]
			case "unfinished":
				events = append(events[:4], events[5])
			case "overflow":
				events[2].ToolCallDelta.ArgumentsDelta = strings.Repeat("x", observedArgumentLimit+1)
				c.Record("torana_search", json.RawMessage(`{"query":"status"}`), Binding{ConversationID: "other", CallID: "other_call"}, time.Now())
			}
			input := make(chan engine.StreamEvent, len(events))
			for _, event := range events {
				input <- event
			}
			close(input)
			for range c.ObserveStream(context.Background(), input, "anthropic", "conversation", []string{"torana"}) {
			}
			if _, ok := c.Consume("torana_search", json.RawMessage(`{"query":"status"}`), time.Now()); ok {
				t.Fatal("failed stream bound")
			}
		})
	}
}

func TestObserveStreamCancellationDoesNotWaitForInputClose(t *testing.T) {
	c := NewCorrelator()
	ctx, cancel := context.WithCancel(context.Background())
	input := make(chan engine.StreamEvent)
	output := c.ObserveStream(ctx, input, "anthropic", "conversation", nil)
	cancel()
	select {
	case _, ok := <-output:
		if ok {
			t.Fatal("unexpected event")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled observer stuck waiting for source")
	}
}
