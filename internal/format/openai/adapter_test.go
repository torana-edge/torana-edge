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

// Ordered-body test helpers (engine.Message).

func textBlock(s string) engine.Block {
	return engine.Block{Text: &engine.TextBlock{Text: s}}
}

func msgBlock(role engine.Role, text string) engine.Message {
	return engine.Message{Role: role, Blocks: []engine.Block{textBlock(text)}}
}

func textOf(m engine.Message) string { return m.Text() }

type tcView struct {
	ID, Name  string
	Args      engine.RequiredJSONObject
	Signature string
}

func toolCalls(m engine.Message) []tcView {
	var out []tcView
	for _, b := range m.Blocks {
		if b.ToolUse != nil {
			out = append(out, tcView{ID: b.ToolUse.ID, Name: b.ToolUse.Name, Args: b.ToolUse.Arguments, Signature: b.ToolUse.Signature})
		}
	}
	return out
}

func toolResults(m engine.Message) []*engine.ToolResultBlock {
	var out []*engine.ToolResultBlock
	for _, b := range m.Blocks {
		if b.ToolResult != nil {
			out = append(out, b.ToolResult)
		}
	}
	return out
}
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
	if textOf(chat.Messages[0]) != "You are helpful." {
		t.Errorf("msg 0 content: got %q", textOf(chat.Messages[0]))
	}

	if chat.Messages[1].Role != engine.RoleUser {
		t.Errorf("msg 1 role: got %s, want user", chat.Messages[1].Role)
	}

	if chat.Messages[2].Role != engine.RoleAssistant {
		t.Errorf("msg 2 role: got %s, want assistant", chat.Messages[2].Role)
	}
	if len(toolCalls(chat.Messages[2])) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls(chat.Messages[2])))
	}
	if got := toolCalls(chat.Messages[2])[0].Name; got != "get_weather" {
		t.Errorf("tool call name: got %s", got)
	}
	vals, _, err := toolCalls(chat.Messages[2])[0].Args.DecodeObject()
	if err != nil {
		t.Fatalf("tool call args decode: %v", err)
	}
	city, ok := vals["city"]
	if !ok || string(city) != `"SF"` {
		t.Errorf("tool call args: got %v", vals)
	}

	if chat.Messages[3].Role != engine.RoleTool {
		t.Errorf("msg 3 role: got %s, want tool", chat.Messages[3].Role)
	}
	if toolResults(chat.Messages[3])[0].ToolCallID != "call_1" {
		t.Errorf("msg 3 tool_call_id: got %s", toolResults(chat.Messages[3])[0].ToolCallID)
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
	if textOf(chat.Messages[0]) != "Hello, what is the weather?" {
		t.Errorf("content: got %q", textOf(chat.Messages[0]))
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

	var extMap map[string]json.RawMessage
	json.Unmarshal(req.ProviderExtensions.Bytes(), &extMap)
	var extInstr string
	json.Unmarshal(extMap["instructions"], &extInstr)
	if extInstr != "Be helpful." {
		t.Errorf("expected instructions to be preserved, got %v", extInstr)
	}
	var extTemp json.Number
	json.Unmarshal(extMap["temperature"], &extTemp)
	if extTemp.String() != "0.5" {
		t.Errorf("expected temperature to be preserved, got %v", extTemp)
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
	if chat.ProviderExtensions.IsAbsent() {
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

	if chat.Messages[0].Role != engine.RoleUser || textOf(chat.Messages[0]) != "Hello!" {
		t.Errorf("message 0 mismatch: %+v", chat.Messages[0])
	}

	tcs := toolCalls(chat.Messages[1])
	if chat.Messages[1].Role != engine.RoleAssistant || len(tcs) != 1 || tcs[0].ID != "call_abc" || tcs[0].Name != "get_weather" {
		t.Errorf("message 1 mismatch: %+v", chat.Messages[1])
	}

	trs := toolResults(chat.Messages[2])
	if chat.Messages[2].Role != engine.RoleTool || len(trs) != 1 || trs[0].ToolCallID != "call_abc" || trs[0].Content[0].Text != "Rainy" {
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
		`data: {"type": "response.completed", "response": {"status": "completed", "usage": {"input_tokens": 10, "output_tokens": 20}}}` + "\n"

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
	if v, err := json.Marshal("responses"); err == nil {
		chat.ProviderExtensions, _ = chat.ProviderExtensions.SetMember("_openai_variant", v)
	}

	var buf bytes.Buffer
	if err := sa.SerializeStream(ctx, &buf, evtCh); err != nil {
		t.Fatal(err)
	}

	serialized := buf.String()
	if !strings.Contains(serialized, "event: response.output_text.delta") || !strings.Contains(serialized, "event: response.function_call_arguments.delta") {
		t.Errorf("serialized output mismatch:\n%s", serialized)
	}
}

// TestSerializeProviderBlockErrorChat: provider blocks have no representation
// on the chat.completion.chunk wire, so a provider BlockStart must error
// explicitly — the pre-fix behavior silently dropped the events. Canonical
// text/thinking blocks are NOT errors: they lower to the delta arms (see
// TestSerializeCanonicalBlocksChat).
func TestSerializeProviderBlockErrorChat(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   engine.StreamEvent
		want string
	}{
		{
			name: "provider block start",
			ev: engine.StreamEvent{BlockStart: &engine.BlockStart{
				Index: 0, Kind: engine.BlockKindProvider, ProviderKind: "redacted",
			}},
			want: `openai: provider block kind "redacted" is not supported by this serializer`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evCh := make(chan engine.StreamEvent, 1)
			evCh <- tc.ev
			close(evCh)
			var buf bytes.Buffer
			err := (&StreamAdapter{}).SerializeStream(context.Background(), &buf, evCh)
			if err == nil {
				t.Fatal("provider block event did not error")
			}
			// The chat serializer wraps its per-event errors with
			// "openai serialize: "; the message itself must carry the
			// explicit support-or-error decision.
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestSerializeCanonicalBlocksChat: text/thinking are canonical Torana
// content, so a plugin-emitted BlockStart(text)+TextDelta+BlockStop (and the
// thinking analogue) must SERIALIZE on the chat protocol, not abort the turn.
// The start/stop boundaries have no chat frame, so they lower to no wire
// content and the deltas ride the content/reasoning_content arms; an empty
// block naturally produces no wire content.
func TestSerializeCanonicalBlocksChat(t *testing.T) {
	for _, tc := range []struct {
		name     string
		events   []engine.StreamEvent
		wantWire []string // substrings that must appear in the SSE output
	}{
		{
			name: "text block",
			events: []engine.StreamEvent{
				{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}},
				{TextDelta: strPtr("hello")},
				{BlockStop: &engine.BlockStop{Index: 0}},
				{FinishReason: "stop"},
			},
			wantWire: []string{`"content":"hello"`},
		},
		{
			name: "thinking block",
			events: []engine.StreamEvent{
				{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindThinking}},
				{ThinkingDelta: strPtr("step by step")},
				{BlockStop: &engine.BlockStop{Index: 0}},
				{FinishReason: "stop"},
			},
			wantWire: []string{`"reasoning_content":"step by step"`},
		},
		{
			name: "empty text block lowers to no wire content",
			events: []engine.StreamEvent{
				{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}},
				{BlockStop: &engine.BlockStop{Index: 0}},
				{FinishReason: "stop"},
			},
			wantWire: []string{`"finish_reason":"stop"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evCh := make(chan engine.StreamEvent, len(tc.events))
			for _, ev := range tc.events {
				evCh <- ev
			}
			close(evCh)
			var buf bytes.Buffer
			if err := (&StreamAdapter{}).SerializeStream(context.Background(), &buf, evCh); err != nil {
				t.Fatalf("canonical block stream must serialize, got error: %v", err)
			}
			for _, want := range tc.wantWire {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("serialized output missing %q:\n%s", want, buf.String())
				}
			}
		})
	}
}

// TestSerializeMalformedBlocksErrorChat: sequence discipline is enforced
// BEFORE lowering — a malformed boundary (second start while open, index
// reuse, stop with no open block) errors, never silently accepted.
func TestSerializeMalformedBlocksErrorChat(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []engine.StreamEvent
	}{
		{
			name: "second start while open",
			events: []engine.StreamEvent{
				{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}},
				{BlockStart: &engine.BlockStart{Index: 1, Kind: engine.BlockKindThinking}},
			},
		},
		{
			name: "index reuse after close",
			events: []engine.StreamEvent{
				{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}},
				{BlockStop: &engine.BlockStop{Index: 0}},
				{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindThinking}},
			},
		},
		{
			name: "stop with no open block",
			events: []engine.StreamEvent{
				{BlockStop: &engine.BlockStop{Index: 0}},
			},
		},
		{
			name: "stop mismatching the open block",
			events: []engine.StreamEvent{
				{BlockStart: &engine.BlockStart{Index: 5, Kind: engine.BlockKindText}},
				{BlockStop: &engine.BlockStop{Index: 3}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evCh := make(chan engine.StreamEvent, len(tc.events))
			for _, ev := range tc.events {
				evCh <- ev
			}
			close(evCh)
			var buf bytes.Buffer
			err := (&StreamAdapter{}).SerializeStream(context.Background(), &buf, evCh)
			if err == nil {
				t.Fatal("malformed block topology did not error")
			}
			if !strings.Contains(err.Error(), "openai:") {
				t.Errorf("error = %q, want an openai: sequence error", err.Error())
			}
		})
	}
}

// TestSerializeCanonicalBlocksResponses: text/thinking are canonical Torana
// content, so BlockStart+delta+BlockStop must serialize on the Responses
// protocol too — boundaries lower to no wire content and the deltas ride the
// output_text.delta arm.
func TestSerializeCanonicalBlocksResponses(t *testing.T) {
	ext, _ := engine.ParseOptionalJSONObject([]byte(`{"_openai_variant":"responses"}`))
	ctx := context.WithValue(context.Background(), engine.ChatRequestKey, &engine.ChatRequest{
		ProviderExtensions: ext,
	})

	for _, tc := range []struct {
		name     string
		events   []engine.StreamEvent
		wantWire []string
	}{
		{
			name: "text block",
			events: []engine.StreamEvent{
				{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}},
				{TextDelta: strPtr("hello")},
				{BlockStop: &engine.BlockStop{Index: 0}},
				{FinishReason: "stop"},
			},
			wantWire: []string{`"type":"response.output_text.delta"`, `"delta":"hello"`},
		},
		{
			name: "thinking block",
			events: []engine.StreamEvent{
				{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindThinking}},
				{ThinkingDelta: strPtr("step by step")},
				{BlockStop: &engine.BlockStop{Index: 0}},
				{FinishReason: "stop"},
			},
			wantWire: []string{`"type":"response.output_text.delta"`, `"delta":"step by step"`},
		},
		{
			name: "empty text block lowers to no wire content",
			events: []engine.StreamEvent{
				{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}},
				{BlockStop: &engine.BlockStop{Index: 0}},
				{FinishReason: "stop"},
			},
			wantWire: []string{`"type":"response.completed"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evCh := make(chan engine.StreamEvent, len(tc.events))
			for _, ev := range tc.events {
				evCh <- ev
			}
			close(evCh)
			var buf bytes.Buffer
			if err := (&StreamAdapter{}).SerializeStream(ctx, &buf, evCh); err != nil {
				t.Fatalf("canonical block stream must serialize, got error: %v", err)
			}
			for _, want := range tc.wantWire {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("serialized output missing %q:\n%s", want, buf.String())
				}
			}
		})
	}
}

// TestSerializeProviderBlockErrorResponses: provider blocks keep the explicit
// error on the Responses serializer — their semantics are genuinely
// unavailable on this protocol.
func TestSerializeProviderBlockErrorResponses(t *testing.T) {
	ext, _ := engine.ParseOptionalJSONObject([]byte(`{"_openai_variant":"responses"}`))
	ctx := context.WithValue(context.Background(), engine.ChatRequestKey, &engine.ChatRequest{
		ProviderExtensions: ext,
	})
	evCh := make(chan engine.StreamEvent, 1)
	evCh <- engine.StreamEvent{BlockStart: &engine.BlockStart{
		Index: 0, Kind: engine.BlockKindProvider, ProviderKind: "redacted",
	}}
	close(evCh)

	var buf bytes.Buffer
	err := (&StreamAdapter{}).SerializeStream(ctx, &buf, evCh)
	if err == nil {
		t.Fatal("provider block start did not error on the responses serializer")
	}
	want := `openai: provider block kind "redacted" is not supported by this serializer`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}
