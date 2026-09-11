package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func TestResponsesCustomToolStreamRoundTrip(t *testing.T) {
	input := strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"id":"ctc_item","type":"custom_tool_call","name":"shell","call_id":"call_7"}}`,
		`data: {"type":"response.custom_tool_call_input.delta","item_id":"ctc_item","delta":"printf "}`,
		`data: {"type":"response.custom_tool_call_input.delta","item_id":"ctc_item","delta":"hello"}`,
		`data: {"type":"response.custom_tool_call_input.done","item_id":"ctc_item","input":"printf hello"}`,
	}, "\n")
	events := collectStreamEvents(input)
	if len(events) != 4 {
		t.Fatalf("events = %+v", events)
	}
	if start := events[0].ToolCallStart; start == nil || start.ID != "call_7" || start.Name != "shell" ||
		start.InvocationKind != engine.ToolInvocationFreeform {
		t.Fatalf("start %+v", start)
	}
	for i, want := range []string{"printf ", "hello"} {
		delta := events[i+1].ToolCallDelta
		if delta == nil || delta.ArgumentsDelta != "" || delta.InputTextDelta == nil || *delta.InputTextDelta != want {
			t.Fatalf("delta %d: %+v", i, delta)
		}
	}
	if events[3].ToolCallEnd == nil || events[3].ToolCallEnd.Index != 0 {
		t.Fatalf("end %+v", events[3])
	}

	eventCh := make(chan engine.StreamEvent, len(events))
	for _, event := range events {
		eventCh <- event
	}
	close(eventCh)
	ctx := context.WithValue(context.Background(), engine.ChatRequestKey, &engine.ChatRequest{OpenAIVariant: engine.OpenAIResponses})
	var out bytes.Buffer
	if err := (&StreamAdapter{}).SerializeStream(ctx, &out, eventCh); err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var object map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &object); err != nil {
			t.Fatal(err)
		}
		types = append(types, object["type"].(string))
		if object["type"] == "response.output_item.done" {
			item := object["item"].(map[string]any)
			if item["type"] != "custom_tool_call" || item["call_id"] != "call_7" || item["name"] != "shell" || item["input"] != "printf hello" {
				t.Fatalf("done item %#v", item)
			}
			if _, exists := item["arguments"]; exists {
				t.Fatalf("custom call gained arguments: %#v", item)
			}
		}
	}
	wantTypes := []string{
		"response.created",
		"response.output_item.added",
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.done",
		"response.output_item.done",
	}
	if strings.Join(types, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("types %v, want %v", types, wantTypes)
	}
}

func TestSerializeResponsesStreamEmitsCompleteLifecycleOnce(t *testing.T) {
	text := "hello"
	events := make(chan engine.StreamEvent, 4)
	events <- engine.StreamEvent{MessageStart: &engine.StreamMessageStart{Role: "assistant", ID: "resp_1", Model: "gpt-x"}}
	events <- engine.StreamEvent{TextDelta: &text}
	events <- engine.StreamEvent{Usage: &engine.StreamUsage{InputTokens: 3, OutputTokens: 2}}
	events <- engine.StreamEvent{FinishReason: "stop"}
	close(events)
	ctx := context.WithValue(context.Background(), engine.ChatRequestKey, &engine.ChatRequest{OpenAIVariant: engine.OpenAIResponses})
	var out bytes.Buffer
	if err := (&StreamAdapter{}).SerializeStream(ctx, &out, events); err != nil {
		t.Fatal(err)
	}
	wire := out.String()
	for _, want := range []string{
		"response.created", "response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.done", "response.content_part.done",
		"response.output_item.done", `"id":"resp_1"`, `"model":"gpt-x"`,
		`"output_index":0`, `"input_tokens":3`, `"output_tokens":2`,
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("wire missing %q:\n%s", want, wire)
		}
	}
	if got := strings.Count(wire, "event: response.completed"); got != 1 {
		t.Fatalf("response.completed count = %d, want 1:\n%s", got, wire)
	}
}

func TestSerializeResponsesStreamPreservesInterleavedOutputOrder(t *testing.T) {
	text := func(value string) engine.StreamEvent { return engine.StreamEvent{TextDelta: &value} }
	tool := func(index int, id, name string) []engine.StreamEvent {
		return []engine.StreamEvent{
			{ToolCallStart: &engine.ToolCallStart{Index: index, ID: id, Name: name}},
			{ToolCallDelta: &engine.ToolCallDelta{Index: index, ArgumentsDelta: `{}`}},
			{ToolCallEnd: &engine.ToolCallEnd{Index: index}},
		}
	}

	tests := []struct {
		name       string
		events     []engine.StreamEvent
		wantTypes  []string
		wantEvents []string
		wantTexts  []string
		wantStatus string
		wantUsage  bool
	}{
		{
			name: "text-tool-text",
			events: append(append([]engine.StreamEvent{text("before")}, tool(0, "call_0", "first")...),
				text("after"), engine.StreamEvent{FinishReason: "stop"}),
			wantTypes: []string{"message", "function_call", "message"},
			wantEvents: []string{
				"response.created",
				"response.output_item.added", "response.content_part.added", "response.output_text.delta",
				"response.output_text.done", "response.content_part.done", "response.output_item.done",
				"response.output_item.added", "response.function_call_arguments.delta",
				"response.function_call_arguments.done", "response.output_item.done",
				"response.output_item.added", "response.content_part.added", "response.output_text.delta",
				"response.output_text.done", "response.content_part.done", "response.output_item.done",
				"response.completed",
			},
			wantTexts:  []string{"before", "after"},
			wantStatus: "completed",
		},
		{
			name: "tool-text-tool-text-incomplete-with-usage",
			events: append(append(append(append(tool(0, "call_0", "first"), text("middle")),
				tool(1, "call_1", "second")...), text("last")),
				engine.StreamEvent{Usage: &engine.StreamUsage{InputTokens: 7, OutputTokens: 5}},
				engine.StreamEvent{FinishReason: "length"}),
			wantTypes: []string{"function_call", "message", "function_call", "message"},
			wantEvents: []string{
				"response.created",
				"response.output_item.added", "response.function_call_arguments.delta",
				"response.function_call_arguments.done", "response.output_item.done",
				"response.output_item.added", "response.content_part.added", "response.output_text.delta",
				"response.output_text.done", "response.content_part.done", "response.output_item.done",
				"response.output_item.added", "response.function_call_arguments.delta",
				"response.function_call_arguments.done", "response.output_item.done",
				"response.output_item.added", "response.content_part.added", "response.output_text.delta",
				"response.output_text.done", "response.content_part.done", "response.output_item.done",
				"response.incomplete",
			},
			wantTexts:  []string{"middle", "last"},
			wantStatus: "incomplete",
			wantUsage:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects := serializeResponsesObjects(t, tc.events)
			gotEvents := make([]string, len(objects))
			for i, object := range objects {
				gotEvents[i] = object["type"].(string)
			}
			if strings.Join(gotEvents, "|") != strings.Join(tc.wantEvents, "|") {
				t.Fatalf("event lifecycle = %v, want %v", gotEvents, tc.wantEvents)
			}
			terminal := objects[len(objects)-1]
			response := terminal["response"].(map[string]any)
			if response["status"] != tc.wantStatus {
				t.Fatalf("status = %v, want %s", response["status"], tc.wantStatus)
			}
			output := response["output"].([]any)
			if len(output) != len(tc.wantTypes) {
				t.Fatalf("output = %#v, want %d items", output, len(tc.wantTypes))
			}
			var gotTexts []string
			for i, raw := range output {
				item := raw.(map[string]any)
				if item["type"] != tc.wantTypes[i] {
					t.Fatalf("output[%d].type = %v, want %s", i, item["type"], tc.wantTypes[i])
				}
				if item["type"] == "message" {
					part := item["content"].([]any)[0].(map[string]any)
					gotTexts = append(gotTexts, part["text"].(string))
				}
			}
			if strings.Join(gotTexts, "|") != strings.Join(tc.wantTexts, "|") {
				t.Fatalf("message texts = %v, want %v", gotTexts, tc.wantTexts)
			}
			_, hasUsage := response["usage"]
			if hasUsage != tc.wantUsage {
				t.Fatalf("usage presence = %v, want %v", hasUsage, tc.wantUsage)
			}
		})
	}
}

func TestSerializeResponsesStreamKeepsSeparateTextBlocks(t *testing.T) {
	first, second := "first", "second"
	events := []engine.StreamEvent{
		{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}},
		{TextDelta: &first},
		{BlockStop: &engine.BlockStop{Index: 0}},
		{BlockStart: &engine.BlockStart{Index: 1, Kind: engine.BlockKindText}},
		{TextDelta: &second},
		{BlockStop: &engine.BlockStop{Index: 1}},
		{FinishReason: "stop"},
	}
	objects := serializeResponsesObjects(t, events)
	response := objects[len(objects)-1]["response"].(map[string]any)
	output := response["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output = %#v, want two message items", output)
	}
	for i, want := range []string{first, second} {
		item := output[i].(map[string]any)
		part := item["content"].([]any)[0].(map[string]any)
		if item["type"] != "message" || part["text"] != want {
			t.Fatalf("output[%d] = %#v, want message %q", i, item, want)
		}
	}
}

func serializeResponsesObjects(t *testing.T, input []engine.StreamEvent) []map[string]any {
	t.Helper()
	events := make(chan engine.StreamEvent, len(input))
	for _, event := range input {
		events <- event
	}
	close(events)
	ctx := context.WithValue(context.Background(), engine.ChatRequestKey, &engine.ChatRequest{OpenAIVariant: engine.OpenAIResponses})
	var out bytes.Buffer
	if err := (&StreamAdapter{}).SerializeStream(ctx, &out, events); err != nil {
		t.Fatal(err)
	}
	var objects []map[string]any
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var object map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &object); err != nil {
			t.Fatal(err)
		}
		objects = append(objects, object)
	}
	return objects
}

func TestResponsesStreamRejectsCrossFamilyDelta(t *testing.T) {
	for _, input := range []string{
		strings.Join([]string{
			`data: {"type":"response.output_item.added","item":{"id":"i","type":"custom_tool_call","name":"shell","call_id":"c"}}`,
			`data: {"type":"response.function_call_arguments.delta","item_id":"i","delta":"{}"}`,
		}, "\n"),
		strings.Join([]string{
			`data: {"type":"response.output_item.added","item":{"id":"i","type":"function_call","name":"f","call_id":"c"}}`,
			`data: {"type":"response.custom_tool_call_input.delta","item_id":"i","delta":"text"}`,
		}, "\n"),
	} {
		events := collectStreamEvents(input)
		if len(events) != 2 || events[1].Error == nil {
			t.Fatalf("cross-family stream did not terminate: %+v", events)
		}
	}
}

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
