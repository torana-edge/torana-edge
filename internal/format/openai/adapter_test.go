package openai

import (
	"bytes"
	"context"
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
	if err := sa.SerializeStream(context.Background(), &buf, evtCh); err != nil {
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

func TestRoundTrip_ResponsesAPI(t *testing.T) {
	adapter := &Adapter{}

	input := `{
		"model": "gpt-4o-realtime",
		"input": [
			{ "type": "message", "role": "user", "content": "Hello!" },
			{ "type": "function_call", "call_id": "call_abc", "name": "get_weather", "arguments": "{\"city\":\"Seattle\"}" },
			{ "type": "function_call_output", "call_id": "call_abc", "output": "Rainy" }
		],
		"tools": [
			{ "type": "function", "name": "get_weather", "description": "Get weather", "parameters": { "type": "object" } }
		],
		"stream": true
	}`

	chat, err := adapter.Unmarshal([]byte(input))
	if err != nil {
		t.Fatal(err)
	}

	if chat.Model != "gpt-4o-realtime" {
		t.Errorf("expected model gpt-4o-realtime, got %q", chat.Model)
	}

	if len(chat.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(chat.Messages))
	}

	if chat.Messages[0].Role != engine.RoleUser || chat.Messages[0].Content != "Hello!" {
		t.Errorf("message 0 mismatch: %+v", chat.Messages[0])
	}

	if chat.Messages[1].Role != engine.RoleAssistant || len(chat.Messages[1].ToolCalls) != 1 || chat.Messages[1].ToolCalls[0].ID != "call_abc" || chat.Messages[1].ToolCalls[0].Name != "get_weather" {
		t.Errorf("message 1 mismatch: %+v", chat.Messages[1])
	}

	if chat.Messages[2].Role != engine.RoleTool || chat.Messages[2].ToolCallID != "call_abc" || chat.Messages[2].Content != "Seattle" {
		// Wait, Seattle or Rainy? Let's check Seattle vs Rainy. Content should be Seattle or output should be Seattle. Output: Seattle is mapped to Content: Seattle.
		// Wait, our test has output: "Rainy", so chat.Messages[2].Content should be "Rainy"!
	}
	if chat.Messages[2].Role != engine.RoleTool || chat.Messages[2].ToolCallID != "call_abc" || chat.Messages[2].Content != "Rainy" {
		t.Errorf("message 2 mismatch: %+v", chat.Messages[2])
	}

	out, err := adapter.Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}

	if parsed["model"] != "gpt-4o-realtime" {
		t.Errorf("marshaled model mismatch: %v", parsed["model"])
	}

	// Test streaming parser
	sa := &StreamAdapter{}
	streamInput := `data: {"type": "response.output_text.delta", "delta": "Hello"}` + "\n" +
		`data: {"type": "response.output_item.added", "item": {"id": "item_123", "type": "function_call", "name": "get_weather", "call_id": "call_123"}}` + "\n" +
		`data: {"type": "response.function_call_arguments.delta", "item_id": "item_123", "delta": "{\""}` + "\n" +
		`data: {"type": "response.function_call_arguments.delta", "item_id": "item_123", "delta": "city\":\"Seattle\"}"}` + "\n" +
		`data: {"type": "response.function_call_arguments.done", "item_id": "item_123"}` + "\n" +
		`data: {"type": "response.completed", "response": {"status": "completed", "usage": {"prompt_tokens": 10, "completion_tokens": 20}}}` + "\n"

	ch := sa.ParseStream(strings.NewReader(streamInput))
	var events []engine.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}

	if len(events) != 7 {
		t.Fatalf("expected 7 events, got %d: %+v", len(events), events)
	}

	if events[0].TextDelta == nil || *events[0].TextDelta != "Hello" {
		t.Errorf("expected TextDelta 'Hello', got %+v", events[0])
	}
	if events[1].ToolCallStart == nil || events[1].ToolCallStart.ID != "call_123" || events[1].ToolCallStart.Name != "get_weather" {
		t.Errorf("expected ToolCallStart, got %+v", events[1])
	}
	if events[2].ToolCallDelta == nil || events[2].ToolCallDelta.ArgumentsDelta != "{\"" {
		t.Errorf("expected ToolCallDelta, got %+v", events[2])
	}
	if events[3].ToolCallDelta == nil || events[3].ToolCallDelta.ArgumentsDelta != "city\":\"Seattle\"}" {
		t.Errorf("expected ToolCallDelta, got %+v", events[3])
	}
	if events[4].ToolCallEnd == nil || events[4].ToolCallEnd.Index != 0 {
		t.Errorf("expected ToolCallEnd, got %+v", events[4])
	}
	if events[5].FinishReason != "stop" {
		t.Errorf("expected FinishReason 'stop', got %+v", events[5])
	}
	if events[6].Usage == nil || events[6].Usage.InputTokens != 10 || events[6].Usage.OutputTokens != 20 {
		t.Errorf("expected Usage, got %+v", events[6])
	}

	// Test streaming serializer
	evtCh := make(chan engine.StreamEvent, len(events))
	for _, ev := range events {
		evtCh <- ev
	}
	close(evtCh)

	ctx := context.WithValue(context.Background(), engine.ChatRequestKey, chat)
	chat.ProviderExtensions["_openai_variant"] = "responses"

	var buf bytes.Buffer
	if err := sa.SerializeStream(ctx, &buf, evtCh); err != nil {
		t.Fatal(err)
	}

	serialized := buf.String()
	if !strings.Contains(serialized, "event: response.output_text.delta") || !strings.Contains(serialized, "event: response.function_call_arguments.delta") {
		t.Errorf("serialized output mismatch:\n%s", serialized)
	}
}
