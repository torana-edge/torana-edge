package openai

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"github.com/torana-edge/torana-edge/internal/engine"
)

func TestRoundTrip_ChatCompletions(t *testing.T) {
	adapter := &Adapter{}

	input := `{
		"model": "gpt-4o",
		"messages": [
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": "What is the weather?"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"SF\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "Sunny, 72F"}
		],
		"tools": [
			{"type": "function", "function": {"name": "get_weather", "description": "Get weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}}}
		],
		"stream": true
	}`

	chat, err := adapter.Unmarshal([]byte(input))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Verify message structure
	if len(chat.Messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(chat.Messages))
	}

	if chat.Messages[0].Role != engine.RoleSystem {
		t.Errorf("msg 0 role: got %s, want system", chat.Messages[0].Role)
	}
	if chat.Messages[0].Content != "You are helpful." {
		t.Errorf("msg 0 content: got %q", chat.Messages[0].Content)
	}

	if chat.Messages[1].Role != engine.RoleUser {
		t.Errorf("msg 1 role: got %s, want user", chat.Messages[1].Role)
	}

	if chat.Messages[2].Role != engine.RoleAssistant {
		t.Errorf("msg 2 role: got %s, want assistant", chat.Messages[2].Role)
	}
	if len(chat.Messages[2].ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(chat.Messages[2].ToolCalls))
	}
	if chat.Messages[2].ToolCalls[0].Name != "get_weather" {
		t.Errorf("tool call name: got %s", chat.Messages[2].ToolCalls[0].Name)
	}
	city, ok := chat.Messages[2].ToolCalls[0].Arguments["city"].(string)
	if !ok || city != "SF" {
		t.Errorf("tool call args: got %v", chat.Messages[2].ToolCalls[0].Arguments)
	}

	if chat.Messages[3].Role != engine.RoleTool {
		t.Errorf("msg 3 role: got %s, want tool", chat.Messages[3].Role)
	}
	if chat.Messages[3].ToolCallID != "call_1" {
		t.Errorf("msg 3 tool_call_id: got %s", chat.Messages[3].ToolCallID)
	}

	if len(chat.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(chat.Tools))
	}

	// Marshal back
	output, err := adapter.Marshal(chat)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(output, &out); err != nil {
		t.Fatalf("remarshal output not valid JSON: %v\n%s", err, output)
	}

	msgs, ok := out["messages"].([]any)
	if !ok || len(msgs) != 4 {
		t.Fatalf("output messages: got %v", out["messages"])
	}
}

func TestUnmarshal_ResponsesAPI(t *testing.T) {
	adapter := &Adapter{}

	// String input
	input := `{"object":"response","input":"Hello, what is the weather?","tools":[{"type":"function","name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}],"stream":true}`

	chat, err := adapter.Unmarshal([]byte(input))
	if err != nil {
		t.Fatalf("unmarshal responses: %v", err)
	}

	if len(chat.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(chat.Messages))
	}
	if chat.Messages[0].Role != engine.RoleUser {
		t.Errorf("role: got %s, want user", chat.Messages[0].Role)
	}
	if chat.Messages[0].Content != "Hello, what is the weather?" {
		t.Errorf("content: got %q", chat.Messages[0].Content)
	}
	if len(chat.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(chat.Tools))
	}
}

func TestStreamParse_ChatCompletions(t *testing.T) {
	sse := `data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"}}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"}}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" world"}}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"SF\"}"}}]}}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]
`

	sa := &StreamAdapter{}
	ch := sa.ParseStream(io.NopCloser(bytes.NewReader([]byte(sse))))

	events := make([]engine.StreamEvent, 0)
	for evt := range ch {
		events = append(events, evt)
	}

	// Expected sequence:
	// 1. TextDelta "Hello"
	// 2. TextDelta " world"
	// 3. ToolCallStart{index:0, id:call_1, name:get_weather}
	// 4. ToolCallDelta{index:0, args:{"city":"SF"}}
	// 5. ToolCallEnd{index:0}
	// 6. FinishReason "tool_calls"

	if len(events) < 6 {
		t.Fatalf("expected at least 6 events, got %d", len(events))
	}

	// Text deltas
	if events[0].TextDelta == nil || *events[0].TextDelta != "Hello" {
		t.Errorf("event 0: expected TextDelta 'Hello', got %+v", events[0])
	}
	if events[1].TextDelta == nil || *events[1].TextDelta != " world" {
		t.Errorf("event 1: expected TextDelta ' world', got %+v", events[1])
	}

	// ToolCallStart
	if events[2].ToolCallStart == nil {
		t.Fatalf("event 2: expected ToolCallStart, got %+v", events[2])
	}
	tcs := events[2].ToolCallStart
	if tcs.Index != 0 || tcs.ID != "call_1" || tcs.Name != "get_weather" {
		t.Errorf("ToolCallStart: got {idx:%d id:%s name:%s}", tcs.Index, tcs.ID, tcs.Name)
	}

	// ToolCallDelta
	if events[3].ToolCallDelta == nil {
		t.Fatalf("event 3: expected ToolCallDelta, got %+v", events[3])
	}
	if events[3].ToolCallDelta.ArgumentsDelta != `{"city":"SF"}` {
		t.Errorf("ToolCallDelta: got %q", events[3].ToolCallDelta.ArgumentsDelta)
	}

	// ToolCallEnd
	if events[4].ToolCallEnd == nil {
		t.Fatalf("event 4: expected ToolCallEnd, got %+v", events[4])
	}

	// FinishReason
	if events[5].FinishReason != "tool_calls" {
		t.Errorf("event 5: expected FinishReason 'tool_calls', got %q", events[5].FinishReason)
	}
}

func TestStreamSerialize_RoundTrip(t *testing.T) {
	sa := &StreamAdapter{}

	events := []engine.StreamEvent{
		{TextDelta: strPtr("Hello")},
		{TextDelta: strPtr(" world")},
		{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "call_1", Name: "get_weather"}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{"city":"SF"}`}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 0}},
		{FinishReason: "tool_calls"},
	}

	evtCh := make(chan engine.StreamEvent, len(events))
	for _, e := range events {
		evtCh <- e
	}
	close(evtCh)

	var buf bytes.Buffer
	if err := sa.SerializeStream(&buf, nil, evtCh); err != nil {
		t.Fatalf("serialize: %v", err)
	}

	output := buf.String()
	t.Logf("serialized:\n%s", output)

	// Parse back
	parsedCh := sa.ParseStream(io.NopCloser(bytes.NewReader([]byte(output))))
	parsed := make([]engine.StreamEvent, 0)
	for evt := range parsedCh {
		parsed = append(parsed, evt)
	}

	// The serializer drops ToolCallEnd, but the parser re-synthesizes it
	// from finish_reason="tool_calls". So we get all 6 events back.
	if len(parsed) != len(events) {
		t.Fatalf("round-trip: expected %d events, got %d", len(events), len(parsed))
	}

	if parsed[0].TextDelta == nil || *parsed[0].TextDelta != "Hello" {
		t.Errorf("rt event 0: got %+v", parsed[0])
	}
	if parsed[2].ToolCallStart == nil || parsed[2].ToolCallStart.Name != "get_weather" {
		t.Errorf("rt event 2: got %+v", parsed[2])
	}
}

func TestStreamParse_StopFinish(t *testing.T) {
	sse := `data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Done"}}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
`
	sa := &StreamAdapter{}
	ch := sa.ParseStream(io.NopCloser(bytes.NewReader([]byte(sse))))

	events := make([]engine.StreamEvent, 0)
	for evt := range ch {
		events = append(events, evt)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[1].FinishReason != "stop" {
		t.Errorf("expected finish_reason 'stop', got %q", events[1].FinishReason)
	}
}

func TestResponsesFieldPreservation(t *testing.T) {
	raw := []byte(`{
		"model": "gpt-4o",
		"instructions": "Be helpful.",
		"temperature": 0.5,
		"input": "Hello"
	}`)

	a := &Adapter{}
	req, err := a.Unmarshal(raw)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if req.ProviderExtensions["instructions"] != "Be helpful." {
		t.Errorf("expected instructions to be preserved, got %v", req.ProviderExtensions["instructions"])
	}
	if req.ProviderExtensions["temperature"] != 0.5 {
		t.Errorf("expected temperature to be preserved, got %v", req.ProviderExtensions["temperature"])
	}

	out, err := a.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var outMap map[string]any
	if err := json.Unmarshal(out, &outMap); err != nil {
		t.Fatalf("Marshal output is not valid JSON: %v\n%s", err, out)
	}
	if outMap["instructions"] != "Be helpful." {
		t.Errorf("expected marshaled instructions to be preserved, got %v", outMap["instructions"])
	}
	if outMap["temperature"] != 0.5 {
		t.Errorf("expected marshaled temperature to be preserved, got %v", outMap["temperature"])
	}
}

func strPtr(s string) *string { return &s }

func TestProviderExtensions_RoundTrip(t *testing.T) {
	adapter := &Adapter{}

	// Known fields (temperature, max_tokens) are handled by the adapter.
	// ProviderExtensions preserve UNKNOWN fields like x-custom-* or response_format.
	input := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"x-custom-metadata":"test-value","stream":true}`

	chat, err := adapter.Unmarshal([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.ProviderExtensions) == 0 {
		t.Fatal("expected ProviderExtensions to be populated for unknown field x-custom-metadata")
	}

	out, err := adapter.Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if v, _ := parsed["x-custom-metadata"].(string); v != "test-value" {
		t.Errorf("x-custom-metadata = %v", v)
	}
	if v, _ := parsed["temperature"].(float64); v != 0.7 {
		t.Errorf("temperature = %v", v)
	}
}

func TestMarshalResponses_MultipleToolCalls(t *testing.T) {
	chat := &engine.ChatRequest{
		Model: "gpt-4o",
		ProviderExtensions: map[string]any{
			"_openai_variant": "responses",
		},
		Messages: []engine.Message{
			{Role: engine.RoleUser, Content: "What's the weather and time?"},
			{Role: engine.RoleAssistant, ToolCalls: []engine.ToolCall{
				{ID: "call_1", Name: "get_weather", Arguments: map[string]any{"city": "NYC"}},
				{ID: "call_2", Name: "get_time", Arguments: map[string]any{"tz": "EST"}},
			}},
			{Role: engine.RoleTool, ToolCallID: "call_1", Content: "Sunny, 72F"},
			{Role: engine.RoleTool, ToolCallID: "call_2", Content: "3:00 PM"},
		},
	}

	adapter := &Adapter{}
	out, err := adapter.Marshal(chat)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}

	input, ok := raw["input"].([]any)
	if !ok {
		t.Fatalf("input is not an array: %T", raw["input"])
	}

	if len(input) != 5 {
		t.Fatalf("expected 5 input items, got %d: %v", len(input), input)
	}

	msg1 := input[1].(map[string]any)
	if msg1["role"] != "assistant" {
		t.Errorf("item 1 role: got %v, want assistant", msg1["role"])
	}
	fc1, ok := msg1["function_call"].(map[string]any)
	if !ok {
		t.Fatalf("item 1 missing function_call: %v", msg1)
	}
	if fc1["call_id"] != "call_1" || fc1["name"] != "get_weather" {
		t.Errorf("item 1 function_call: call_id=%v name=%v", fc1["call_id"], fc1["name"])
	}

	msg2 := input[2].(map[string]any)
	fc2, ok := msg2["function_call"].(map[string]any)
	if !ok {
		t.Fatalf("item 2 missing function_call: %v", msg2)
	}
	if fc2["call_id"] != "call_2" || fc2["name"] != "get_time" {
		t.Errorf("item 2 function_call: call_id=%v name=%v", fc2["call_id"], fc2["name"])
	}

	msg3 := input[3].(map[string]any)
	fco1, ok := msg3["function_call_output"].(map[string]any)
	if !ok {
		t.Fatalf("item 3 missing function_call_output: %v", msg3)
	}
	if fco1["call_id"] != "call_1" || fco1["output"] != "Sunny, 72F" {
		t.Errorf("item 3 function_call_output: call_id=%v output=%v", fco1["call_id"], fco1["output"])
	}
}

func TestMarshalResponses_NoToolNameFallback(t *testing.T) {
	chat := &engine.ChatRequest{
		Model: "gpt-4o",
		ProviderExtensions: map[string]any{
			"_openai_variant": "responses",
		},
		Messages: []engine.Message{
			{Role: engine.RoleUser, Content: "hi"},
			{Role: engine.RoleTool, ToolName: "some_tool", Content: "result without id"},
		},
	}

	adapter := &Adapter{}
	out, err := adapter.Marshal(chat)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}

	input, _ := raw["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("expected 1 input item, got %d", len(input))
	}
}

func TestStreamSerialize_ResponsesAPI(t *testing.T) {
	chat := &engine.ChatRequest{
		ProviderExtensions: map[string]any{
			"_openai_variant": "responses",
		},
	}

	events := []engine.StreamEvent{
		{TextDelta: strPtr("Hello")},
		{TextDelta: strPtr(" world")},
		{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "call_1", Name: "get_weather"}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{"city":"SF"}`}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 0}},
		{FinishReason: "stop"},
	}

	evtCh := make(chan engine.StreamEvent, len(events))
	for _, e := range events {
		evtCh <- e
	}
	close(evtCh)

	sa := &StreamAdapter{}
	var buf bytes.Buffer
	if err := sa.SerializeStream(&buf, chat, evtCh); err != nil {
		t.Fatalf("serialize: %v", err)
	}

	output := buf.String()
	t.Logf("Responses API serialized:\n%s", output)

	if !strings.Contains(output, `"type":"response.text.delta"`) {
		t.Error("missing response.text.delta events")
	}
	if !strings.Contains(output, `"call_id":"call_1"`) {
		t.Error("first function_call delta missing call_id")
	}
	if !strings.Contains(output, `"name":"get_weather"`) {
		t.Error("first function_call delta missing name")
	}
	if !strings.Contains(output, `"type":"response.function_call.arguments.done"`) {
		t.Error("missing response.function_call.arguments.done")
	}
	if !strings.Contains(output, `"type":"response.done"`) {
		t.Error("missing response.done")
	}
	count := strings.Count(output, `response.function_call.arguments.delta`)
	if count != 1 {
		t.Errorf("expected exactly 1 function_call.arguments.delta event, got %d", count)
	}
}

func TestStreamParse_ResponsesAPI(t *testing.T) {
	sse := `data: {"type":"response.text.delta","delta":"Hello"}
data: {"type":"response.text.delta","delta":" world"}
data: {"type":"response.function_call.arguments.delta","call_id":"call_1","name":"get_weather","delta":"{\"city\":\"SF\"}"}
data: {"type":"response.function_call.arguments.done"}
data: {"type":"response.done"}
`

	sa := &StreamAdapter{}
	ch := sa.ParseStream(io.NopCloser(bytes.NewReader([]byte(sse))))

	events := make([]engine.StreamEvent, 0)
	for evt := range ch {
		events = append(events, evt)
	}

	if len(events) < 6 {
		t.Fatalf("expected at least 6 events, got %d: %+v", len(events), events)
	}

	if events[0].TextDelta == nil || *events[0].TextDelta != "Hello" {
		t.Errorf("event 0: expected TextDelta 'Hello', got %+v", events[0])
	}
	if events[1].TextDelta == nil || *events[1].TextDelta != " world" {
		t.Errorf("event 1: expected TextDelta ' world', got %+v", events[1])
	}

	if events[2].ToolCallStart == nil {
		t.Fatalf("event 2: expected ToolCallStart, got %+v", events[2])
	}
	tcs := events[2].ToolCallStart
	if tcs.ID != "call_1" || tcs.Name != "get_weather" {
		t.Errorf("ToolCallStart: got id=%s name=%s", tcs.ID, tcs.Name)
	}

	if events[3].ToolCallDelta == nil {
		t.Fatalf("event 3: expected ToolCallDelta, got %+v", events[3])
	}
	if events[3].ToolCallDelta.ArgumentsDelta != `{"city":"SF"}` {
		t.Errorf("ToolCallDelta: got %q", events[3].ToolCallDelta.ArgumentsDelta)
	}

	if events[4].ToolCallEnd == nil {
		t.Fatalf("event 4: expected ToolCallEnd, got %+v", events[4])
	}

	if events[5].FinishReason != "stop" {
		t.Errorf("event 5: expected FinishReason 'stop', got %q", events[5].FinishReason)
	}
}
