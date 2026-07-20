package openai

import (
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func collectStreamEvents(input string) []engine.StreamEvent {
	var events []engine.StreamEvent
	for event := range (&StreamAdapter{}).ParseStream(strings.NewReader(input)) {
		events = append(events, event)
	}
	return events
}

func TestParseResponsesStreamCompactOfficialEvents(t *testing.T) {
	input := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"Hello"}`,
		`data: {"type":"response.output_item.added","item":{"id":"item_1","type":"function_call","name":"lookup","call_id":"call_1"}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_1","delta":"{\"q\":"}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_1","delta":"\"torana\"}"}`,
		`data: {"type":"response.function_call_arguments.done","item_id":"item_1"}`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`,
	}, "\n")

	events := collectStreamEvents(input)
	if len(events) != 6 {
		t.Fatalf("expected 6 events, got %d: %+v", len(events), events)
	}
	if events[0].TextDelta == nil || *events[0].TextDelta != "Hello" {
		t.Fatalf("event 0: expected output text delta, got %+v", events[0])
	}
	if got := events[1].ToolCallStart; got == nil || got.Index != 0 || got.ID != "call_1" || got.Name != "lookup" {
		t.Fatalf("event 1: unexpected tool call start: %+v", events[1])
	}
	if got := events[2].ToolCallDelta; got == nil || got.Index != 0 || got.ArgumentsDelta != `{"q":` {
		t.Fatalf("event 2: unexpected tool call delta: %+v", events[2])
	}
	if got := events[3].ToolCallDelta; got == nil || got.Index != 0 || got.ArgumentsDelta != `"torana"}` {
		t.Fatalf("event 3: unexpected tool call delta: %+v", events[3])
	}
	if got := events[4].ToolCallEnd; got == nil || got.Index != 0 {
		t.Fatalf("event 4: unexpected tool call end: %+v", events[4])
	}
	if events[5].FinishReason != "stop" {
		t.Fatalf("event 5: expected stop, got %+v", events[5])
	}
}

func TestParseResponsesStreamParallelToolCalls(t *testing.T) {
	// Deltas and completion events are deliberately interleaved. Argument
	// events carry item_id (not call_id/name), matching the Responses API.
	input := strings.Join([]string{
		`data: { "type" : "response.output_item.added", "item" : {"id":"item_weather","type":"function_call","name":"weather","call_id":"call_weather"}}`,
		`data: {"type":"response.output_item.added","item":{"id":"item_time","type":"function_call","name":"time","call_id":"call_time"}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_time","delta":"{\"tz\":"}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_weather","delta":"{\"city\":"}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_time","delta":"\"UTC\"}"}`,
		`data: {"type":"response.function_call_arguments.done","item_id":"item_time"}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_weather","delta":"\"Pune\"}"}`,
		`data: {"type":"response.function_call_arguments.done","item_id":"item_weather"}`,
	}, "\n")

	events := collectStreamEvents(input)
	if len(events) != 8 {
		t.Fatalf("expected 8 events, got %d: %+v", len(events), events)
	}

	wantStarts := []struct {
		index int
		id    string
		name  string
	}{{0, "call_weather", "weather"}, {1, "call_time", "time"}}
	for i, want := range wantStarts {
		got := events[i].ToolCallStart
		if got == nil || got.Index != want.index || got.ID != want.id || got.Name != want.name {
			t.Fatalf("start %d: got %+v, want %+v", i, got, want)
		}
	}

	wantRest := []struct {
		kind  string
		index int
		delta string
	}{
		{"delta", 1, `{"tz":`},
		{"delta", 0, `{"city":`},
		{"delta", 1, `"UTC"}`},
		{"end", 1, ""},
		{"delta", 0, `"Pune"}`},
		{"end", 0, ""},
	}
	for i, want := range wantRest {
		got := events[i+2]
		if want.kind == "delta" {
			if got.ToolCallDelta == nil || got.ToolCallDelta.Index != want.index || got.ToolCallDelta.ArgumentsDelta != want.delta {
				t.Fatalf("event %d: got %+v, want %+v", i+2, got, want)
			}
		} else if got.ToolCallEnd == nil || got.ToolCallEnd.Index != want.index {
			t.Fatalf("event %d: got %+v, want %+v", i+2, got, want)
		}
	}
}

func TestParseResponsesStreamIgnoresUnknownToolItemIDs(t *testing.T) {
	input := strings.Join([]string{
		`data: {"type":"response.function_call_arguments.delta","item_id":"unknown","delta":"{}"}`,
		`data: {"type":"response.function_call_arguments.done","item_id":"unknown"}`,
	}, "\n")

	if events := collectStreamEvents(input); len(events) != 0 {
		t.Fatalf("unknown item IDs must not be assigned to another call: %+v", events)
	}
}

func TestParseResponsesStreamOfficialUsageAndCacheDetails(t *testing.T) {
	input := `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":12000,"output_tokens":345,"total_tokens":12345,"input_tokens_details":{"cached_tokens":9000,"cache_write_tokens":1500}}}}` + "\n"

	events := collectStreamEvents(input)
	if len(events) != 2 {
		t.Fatalf("expected finish and usage events, got %d: %+v", len(events), events)
	}
	if events[0].FinishReason != "stop" {
		t.Fatalf("event 0: expected stop, got %+v", events[0])
	}
	usage := events[1].Usage
	if usage == nil {
		t.Fatalf("event 1: expected usage, got %+v", events[1])
	}
	if usage.InputTokens != 12000 || usage.OutputTokens != 345 || usage.CacheReadTokens != 9000 || usage.CacheWriteTokens != 1500 {
		t.Fatalf("unexpected Responses usage: %+v", usage)
	}
}
