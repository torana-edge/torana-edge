package anthropic

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func TestRoundTrip(t *testing.T) {
	adapter := &Adapter{}

	// Anthropic Messages request with system, user text, assistant tool_use, tool_result.
	input := `{
		"model": "claude-sonnet-4-20250514",
		"max_tokens": 1024,
		"system": [
			{"type": "text", "text": "You are helpful."}
		],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "What is the weather?"}]},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "SF"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "Sunny, 72F"}
			]}
		],
		"tools": [
			{"name": "get_weather", "description": "Get weather", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}
		],
		"stream": true
	}`

	chat, err := adapter.Unmarshal([]byte(input))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Verify message structure.
	// Expected: system, user, assistant (tool_use), tool (tool_result)
	if len(chat.Messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(chat.Messages))
	}

	// System message.
	if chat.Messages[0].Role != engine.RoleSystem {
		t.Errorf("msg 0 role: got %s, want system", chat.Messages[0].Role)
	}
	if chat.Messages[0].Content != "You are helpful." {
		t.Errorf("msg 0 content: got %q", chat.Messages[0].Content)
	}

	// User message.
	if chat.Messages[1].Role != engine.RoleUser {
		t.Errorf("msg 1 role: got %s, want user", chat.Messages[1].Role)
	}
	if chat.Messages[1].Content != "What is the weather?" {
		t.Errorf("msg 1 content: got %q", chat.Messages[1].Content)
	}

	// Assistant message with tool call.
	if chat.Messages[2].Role != engine.RoleAssistant {
		t.Errorf("msg 2 role: got %s, want assistant", chat.Messages[2].Role)
	}
	if len(chat.Messages[2].ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(chat.Messages[2].ToolCalls))
	}
	tc := chat.Messages[2].ToolCalls[0]
	if tc.ID != "toolu_1" {
		t.Errorf("tool call id: got %s, want toolu_1", tc.ID)
	}
	if tc.Name != "get_weather" {
		t.Errorf("tool call name: got %s, want get_weather", tc.Name)
	}
	city, ok := tc.Arguments["city"].(string)
	if !ok || city != "SF" {
		t.Errorf("tool call args: got %v", tc.Arguments)
	}

	// Tool result message.
	if chat.Messages[3].Role != engine.RoleTool {
		t.Errorf("msg 3 role: got %s, want tool", chat.Messages[3].Role)
	}
	if chat.Messages[3].ToolCallID != "toolu_1" {
		t.Errorf("msg 3 tool_call_id: got %s, want toolu_1", chat.Messages[3].ToolCallID)
	}
	if chat.Messages[3].Content != "Sunny, 72F" {
		t.Errorf("msg 3 content: got %q", chat.Messages[3].Content)
	}

	// Tool definitions.
	if len(chat.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(chat.Tools))
	}
	if chat.Tools[0].Name != "get_weather" {
		t.Errorf("tool name: got %s, want get_weather", chat.Tools[0].Name)
	}
	if chat.Tools[0].Description != "Get weather" {
		t.Errorf("tool description: got %s", chat.Tools[0].Description)
	}

	if !chat.Stream {
		t.Error("expected Stream to be true")
	}

	// Marshal back.
	output, err := adapter.Marshal(chat)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(output, &out); err != nil {
		t.Fatalf("remarshal output not valid JSON: %v\n%s", err, output)
	}

	// Verify system array.
	sys, ok := out["system"].([]any)
	if !ok || len(sys) != 1 {
		t.Fatalf("output system: got %v", out["system"])
	}

	// Verify messages array.
	msgs, ok := out["messages"].([]any)
	if !ok || len(msgs) != 3 {
		t.Fatalf("output messages: got %v (len=%d)", out["messages"], len(msgs))
	}

	// Verify tools.
	tools, ok := out["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("output tools: got %v", out["tools"])
	}
}

func TestStreamParse(t *testing.T) {
	sse := `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}

data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"SF\"}"}}

data: {"type":"content_block_stop","index":1}

data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null}}

data: {"type":"message_stop"}
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
	// 3. ToolCallStart{index:1, id:toolu_1, name:get_weather}
	// 4. ToolCallDelta{index:1, args:{"city":}
	// 5. ToolCallDelta{index:1, args:SF\"}}
	// 6. ToolCallEnd{index:1}
	// 7. FinishReason "tool_calls"
	if len(events) != 7 {
		t.Fatalf("expected 7 events, got %d: %+v", len(events), events)
	}

	// Text deltas.
	if events[0].TextDelta == nil || *events[0].TextDelta != "Hello" {
		t.Errorf("event 0: expected TextDelta 'Hello', got %+v", events[0])
	}
	if events[1].TextDelta == nil || *events[1].TextDelta != " world" {
		t.Errorf("event 1: expected TextDelta ' world', got %+v", events[1])
	}

	// ToolCallStart.
	if events[2].ToolCallStart == nil {
		t.Fatalf("event 2: expected ToolCallStart, got %+v", events[2])
	}
	tcs := events[2].ToolCallStart
	if tcs.Index != 1 || tcs.ID != "toolu_1" || tcs.Name != "get_weather" {
		t.Errorf("ToolCallStart: got {idx:%d id:%s name:%s}", tcs.Index, tcs.ID, tcs.Name)
	}

	// ToolCallDelta 1.
	if events[3].ToolCallDelta == nil {
		t.Fatalf("event 3: expected ToolCallDelta, got %+v", events[3])
	}
	if events[3].ToolCallDelta.ArgumentsDelta != `{"city":` {
		t.Errorf("ToolCallDelta[0]: got %q", events[3].ToolCallDelta.ArgumentsDelta)
	}

	// ToolCallDelta 2.
	if events[4].ToolCallDelta == nil {
		t.Fatalf("event 4: expected ToolCallDelta, got %+v", events[4])
	}
	if events[4].ToolCallDelta.ArgumentsDelta != `SF"}` {
		t.Errorf("ToolCallDelta[1]: got %q", events[4].ToolCallDelta.ArgumentsDelta)
	}

	// ToolCallEnd.
	if events[5].ToolCallEnd == nil {
		t.Fatalf("event 5: expected ToolCallEnd, got %+v", events[5])
	}
	if events[5].ToolCallEnd.Index != 1 {
		t.Errorf("ToolCallEnd index: got %d", events[5].ToolCallEnd.Index)
	}

	// FinishReason.
	if events[6].FinishReason != "tool_calls" {
		t.Errorf("event 6: expected FinishReason 'tool_calls', got %q", events[6].FinishReason)
	}
}

func TestStreamSerialize(t *testing.T) {
	sa := &StreamAdapter{}
	events := []engine.StreamEvent{
		{TextDelta: strPtr("Hello")},
		{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "toolu_1", Name: "get_weather"}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{"city":"SF"}`}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 0}},
		{FinishReason: "stop"},
	}

	evtCh := make(chan engine.StreamEvent, len(events))
	for _, e := range events {
		evtCh <- e
	}
	close(evtCh)

	var buf bytes.Buffer
	if err := sa.SerializeStream(&buf, evtCh); err != nil {
		t.Fatalf("serialize: %v", err)
	}

	output := buf.String()
	t.Logf("serialized output:\n%s", output)

	// Verify expected SSE lines are present.
	if !strings.Contains(output, `"text_delta"`) {
		t.Error("output missing text_delta")
	}
	if !strings.Contains(output, `"Hello"`) {
		t.Error("output missing Hello text")
	}
	if !strings.Contains(output, `"tool_use"`) {
		t.Error("output missing tool_use content block")
	}
	if !strings.Contains(output, `"get_weather"`) {
		t.Error("output missing get_weather tool name")
	}
	if !strings.Contains(output, `"input_json_delta"`) {
		t.Error("output missing input_json_delta")
	}
	if !strings.Contains(output, `content_block_stop`) {
		t.Error("output missing content_block_stop")
	}
	if !strings.Contains(output, `"stop_reason":"end_turn"`) {
		t.Errorf("output missing end_turn stop_reason: %s", output)
	}
	if !strings.Contains(output, `message_stop`) {
		t.Error("output missing message_stop")
	}

	// Parse back and verify event sequence.
	parsedCh := sa.ParseStream(io.NopCloser(bytes.NewReader([]byte(output))))
	parsed := make([]engine.StreamEvent, 0)
	for evt := range parsedCh {
		parsed = append(parsed, evt)
	}

	if len(parsed) != len(events) {
		t.Fatalf("round-trip: expected %d events, got %d", len(events), len(parsed))
	}

	if parsed[0].TextDelta == nil || *parsed[0].TextDelta != "Hello" {
		t.Errorf("rt event 0: expected TextDelta 'Hello', got %+v", parsed[0])
	}
	if parsed[1].ToolCallStart == nil || parsed[1].ToolCallStart.Name != "get_weather" {
		t.Errorf("rt event 1: expected ToolCallStart get_weather, got %+v", parsed[1])
	}
	if parsed[2].ToolCallDelta == nil || parsed[2].ToolCallDelta.ArgumentsDelta != `{"city":"SF"}` {
		t.Errorf("rt event 2: expected ToolCallDelta, got %+v", parsed[2])
	}
	if parsed[3].ToolCallEnd == nil {
		t.Errorf("rt event 3: expected ToolCallEnd, got %+v", parsed[3])
	}
	if parsed[4].FinishReason != "stop" {
		t.Errorf("rt event 4: expected FinishReason 'stop', got %q", parsed[4].FinishReason)
	}
}

func strPtr(s string) *string { return &s }

// TestParallelToolResultsCoalesce is a regression test for the Anthropic
// request-serialization bug that broke Claude Code: a single assistant turn
// with multiple (parallel) tool_use blocks must be answered by tool_result
// blocks in the ONE immediately-following user message. The canonical IR
// represents each tool result as its own engine.RoleTool message, so Marshal
// must coalesce a consecutive run of them into a single Anthropic user
// message. Emitting one user message per result yields:
//
//	messages.N: `tool_use` ids were found without `tool_result` blocks
//	immediately after ... (HTTP 400 from Anthropic/DeepSeek)
func TestParallelToolResultsCoalesce(t *testing.T) {
	adapter := &Adapter{}

	chat := &engine.ChatRequest{
		Model:     "claude-x",
		MaxTokens: intPtr(256),
		Messages: []engine.Message{
			{Role: engine.RoleUser, Content: "read both files"},
			{Role: engine.RoleAssistant, ToolCalls: []engine.ToolCall{
				{ID: "toolu_a", Name: "read", Arguments: map[string]any{"path": "a.go"}},
				{ID: "toolu_b", Name: "read", Arguments: map[string]any{"path": "b.go"}},
			}},
			{Role: engine.RoleTool, ToolCallID: "toolu_a", Content: "package alpha"},
			{Role: engine.RoleTool, ToolCallID: "toolu_b", Content: "package beta"},
			{Role: engine.RoleUser, Content: "thanks"},
		},
	}

	out, err := adapter.Marshal(chat)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}

	// Expected shape: user(text), assistant(2 tool_use), user(2 tool_result), user(text).
	if len(got.Messages) != 4 {
		roles := make([]string, len(got.Messages))
		for i, m := range got.Messages {
			roles[i] = m.Role
		}
		t.Fatalf("expected 4 messages after coalescing, got %d: %v", len(got.Messages), roles)
	}

	// The tool_result message (index 2) must carry BOTH results, in order.
	tr := got.Messages[2]
	if tr.Role != "user" {
		t.Fatalf("tool-result message role: got %s, want user", tr.Role)
	}
	var ids []string
	for _, b := range tr.Content {
		if b.Type == "tool_result" {
			ids = append(ids, b.ToolUseID)
		}
	}
	if len(ids) != 2 || ids[0] != "toolu_a" || ids[1] != "toolu_b" {
		t.Fatalf("coalesced tool_result ids: got %v, want [toolu_a toolu_b]", ids)
	}

	// A following non-tool message must NOT be merged into the tool-result turn.
	if got.Messages[3].Role != "user" || len(got.Messages[3].Content) == 0 || got.Messages[3].Content[0].Type != "text" {
		t.Fatalf("message after tool results: got %+v, want user/text", got.Messages[3])
	}
}

// TestToolUseAlwaysHasInput: a tool call with no arguments must still
// serialize `"input":{}` — Anthropic requires the field on every tool_use
// block. Without it the API rejects the request with "missing field `input`".
// Regression: found multi-turn during dogfooding (a replayed no-arg tool call,
// or one the intent plugin stripped down to {}, produced an invalid block).
func TestToolUseAlwaysHasInput(t *testing.T) {
	adapter := &Adapter{}

	cases := map[string]map[string]any{
		"nil args":   nil,
		"empty args": {},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			chat := &engine.ChatRequest{
				Model:     "claude-x",
				MaxTokens: intPtr(64),
				Messages: []engine.Message{
					{Role: engine.RoleUser, Content: "list files"},
					{Role: engine.RoleAssistant, ToolCalls: []engine.ToolCall{
						{ID: "toolu_1", Name: "list_files", Arguments: args},
					}},
					{Role: engine.RoleTool, ToolCallID: "toolu_1", Content: "ok"},
				},
			}
			out, err := adapter.Marshal(chat)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			var got struct {
				Messages []struct {
					Content []struct {
						Type  string          `json:"type"`
						Input json.RawMessage `json:"input"`
					} `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("output not valid JSON: %v\n%s", err, out)
			}
			var found bool
			for _, m := range got.Messages {
				for _, b := range m.Content {
					if b.Type != "tool_use" {
						continue
					}
					found = true
					if len(b.Input) == 0 {
						t.Fatalf("tool_use block missing input field: %s", out)
					}
					if string(b.Input) != "{}" {
						t.Fatalf("empty-args tool_use input: got %s, want {}", b.Input)
					}
				}
			}
			if !found {
				t.Fatalf("no tool_use block emitted: %s", out)
			}
		})
	}
}

func intPtr(i int) *int { return &i }
