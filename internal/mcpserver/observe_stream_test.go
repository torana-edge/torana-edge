package mcpserver

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format/gemini"
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
			for event := range c.ObserveStream(context.Background(), input, shape, "conversation", "host-request", []string{"torana"}, nil) {
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
			for range c.ObserveStream(context.Background(), input, "anthropic", "conversation", "host-request", []string{"torana"}, nil) {
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
	output := c.ObserveStream(ctx, input, "anthropic", "conversation", "host-request", nil, func() { close(input) })
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

func TestObserveNativeGeminiStreamsDoNotReuseFunctionNameIDs(t *testing.T) {
	for _, shape := range []string{"gemini", "gemini-codeassist"} {
		c := NewCorrelator()
		for _, query := range []string{"first", "second"} {
			wire := `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"torana_search","args":{"query":"` + query + `"}}}]}}]}`
			if shape == "gemini-codeassist" {
				wire = `{"response":` + wire + `}`
			}
			adapter := gemini.StreamAdapter{Wrapped: shape == "gemini-codeassist"}
			input := adapter.ParseStream(strings.NewReader("data: " + wire + "\n\n"))
			for range c.ObserveStream(context.Background(), input, shape, "conversation", "request-"+query, nil, nil) {
			}
		}
		for _, query := range []string{"first", "second"} {
			binding, ok := c.Consume("torana_search", json.RawMessage(`{"query":"`+query+`"}`), time.Now())
			if !ok || binding.CallID != "gemini:request-"+query+":0" {
				t.Fatalf("%s: binding=%+v ok=%t", shape, binding, ok)
			}
		}
	}
}

func TestStreamOverflowOnlySaturatesAffectedTool(t *testing.T) {
	c := NewCorrelator()
	now := time.Now()
	c.Record("torana_namespaces", json.RawMessage(`{}`), Binding{ConversationID: "other", CallID: "safe"}, now)
	c.Record("torana_search", json.RawMessage(`{"query":"status"}`), Binding{ConversationID: "other", CallID: "ambiguous"}, now)
	events := observationEvents()
	// Whitespace can normalize to an MCP request well below its 64 KiB
	// transport cap, so ignoring oversized evidence would be unsafe.
	events[2].ToolCallDelta.ArgumentsDelta = strings.Repeat(" ", observedArgumentLimit) + `{"query":"status"}`
	input := make(chan engine.StreamEvent, len(events))
	for _, event := range events {
		input <- event
	}
	close(input)
	for range c.ObserveStream(context.Background(), input, "anthropic", "conversation", "request", []string{"torana"}, nil) {
	}
	if _, ok := c.Consume("torana_search", json.RawMessage(`{"query":"status"}`), now); ok {
		t.Fatal("overflow hid ambiguity")
	}
	if _, ok := c.Consume("torana_namespaces", json.RawMessage(`{}`), now); !ok {
		t.Fatal("unrelated tool was saturated")
	}
}

func TestStreamCancellationStopsAndDrainsUnbufferedProducer(t *testing.T) {
	c := NewCorrelator()
	ctx, cancel := context.WithCancel(context.Background())
	input := make(chan engine.StreamEvent)
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer close(input)
		for _, event := range observationEvents() {
			input <- event
		}
	}()
	output := c.ObserveStream(ctx, input, "anthropic", "conversation", "request", nil, func() { close(stopped) })
	cancel()
	for range output {
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("source not stopped")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("producer stranded on input send")
	}
}
