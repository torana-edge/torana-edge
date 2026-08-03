package anthropic

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

func hasCacheBlock(m engine.Message) bool {
	for _, b := range m.Blocks {
		if b.CacheBreakpoint != nil {
			return true
		}
	}
	return false
}

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
func mustReqArgs(t *testing.T, raw string) engine.RequiredJSONObject {
	t.Helper()
	r, err := engine.ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// mustReqOrEmpty uses the normalization constructor: empty means the
// canonical {}.
func mustReqOrEmpty(t *testing.T, raw string) engine.RequiredJSONObject {
	t.Helper()
	r, err := engine.ParseRequiredObjectOrEmpty([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

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
	if textOf(chat.Messages[0]) != "You are helpful." {
		t.Errorf("msg 0 content: got %q", textOf(chat.Messages[0]))
	}

	// User message.
	if chat.Messages[1].Role != engine.RoleUser {
		t.Errorf("msg 1 role: got %s, want user", chat.Messages[1].Role)
	}
	if textOf(chat.Messages[1]) != "What is the weather?" {
		t.Errorf("msg 1 content: got %q", textOf(chat.Messages[1]))
	}

	// Assistant message with tool call.
	if chat.Messages[2].Role != engine.RoleAssistant {
		t.Errorf("msg 2 role: got %s, want assistant", chat.Messages[2].Role)
	}
	if len(toolCalls(chat.Messages[2])) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls(chat.Messages[2])))
	}
	tc := toolCalls(chat.Messages[2])[0]
	if tc.ID != "toolu_1" {
		t.Errorf("tool call id: got %s, want toolu_1", tc.ID)
	}
	if tc.Name != "get_weather" {
		t.Errorf("tool call name: got %s, want get_weather", tc.Name)
	}
	vals, _, err := tc.Args.DecodeObject()
	if err != nil {
		t.Fatalf("tool call args decode: %v", err)
	}
	city, ok := vals["city"]
	if !ok || string(city) != `"SF"` {
		t.Errorf("tool call args: got %v", vals)
	}

	// Tool result: anthropic has no tool role — the result rides its user
	// message at its exact wire position (no synthetic RoleTool split).
	if chat.Messages[3].Role != engine.RoleUser {
		t.Errorf("msg 3 role: got %s, want user (result rides its user message)", chat.Messages[3].Role)
	}
	trs := toolResults(chat.Messages[3])
	if len(trs) != 1 || trs[0].ToolCallID != "toolu_1" {
		t.Errorf("msg 3 tool result: got %+v, want toolu_1", trs)
	}
	if len(trs[0].Content) != 1 || trs[0].Content[0].Text != "Sunny, 72F" {
		t.Errorf("msg 3 result content: got %+v", trs[0].Content)
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
	if err := sa.SerializeStream(context.Background(), &buf, evtCh); err != nil {
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
			{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "read both files"}}}},
			{Role: engine.RoleAssistant, Blocks: []engine.Block{
				{ToolUse: &engine.ToolUseBlock{ID: "toolu_a", Name: "read", Arguments: mustReqArgs(t, `{"path": "a.go"}`)}},
				{ToolUse: &engine.ToolUseBlock{ID: "toolu_b", Name: "read", Arguments: mustReqArgs(t, `{"path": "b.go"}`)}},
			}},
			{Role: engine.RoleUser, Blocks: []engine.Block{
				{ToolResult: &engine.ToolResultBlock{
					ToolCallID: "toolu_a", Content: []engine.ToolResultContentBlock{{Text: "package alpha"}},
				}},
				{ToolResult: &engine.ToolResultBlock{
					ToolCallID: "toolu_b", Content: []engine.ToolResultContentBlock{{Text: "package beta"}},
				}},
			}},
			{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "thanks"}}}},
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

	cases := map[string]string{
		"nil args":   "",
		"empty args": "{}",
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			chat := &engine.ChatRequest{
				Model:     "claude-x",
				MaxTokens: intPtr(64),
				Messages: []engine.Message{
					{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "list files"}}}},
					{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{
						ID: "toolu_1", Name: "list_files", Arguments: mustReqOrEmpty(t, args),
					}}}},
					{Role: engine.RoleUser, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{
						ToolCallID: "toolu_1", Content: []engine.ToolResultContentBlock{{Text: "ok"}},
					}}}},
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

// TestToolResultsStayAdjacentToToolUse: Claude Code sends the tool-results
// user message as [tool_result..., text] — the text is its injected context.
// The IR round trip must keep the results IMMEDIATELY after the assistant's
// tool_use turn; flattening the text first interposed a user message between
// tool_use and tool_result, which strict providers 400 on ("tool_use ids
// were found without tool_result blocks immediately after"). Caught live:
// the error killed every parallel-subagent Claude Code session.
func TestToolResultsStayAdjacentToToolUse(t *testing.T) {
	adapter := &Adapter{}

	inbound := []byte(`{
		"model": "claude-x",
		"max_tokens": 128,
		"messages": [
			{"role": "user", "content": "run both probes"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "probing", "signature": ""},
				{"type": "tool_use", "id": "tu_1", "name": "probe", "input": {"q": "a"}},
				{"type": "tool_use", "id": "tu_2", "name": "probe", "input": {"q": "b"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tu_1", "content": "ok1"},
				{"type": "tool_result", "tool_use_id": "tu_2", "content": "ok2"},
				{"type": "text", "text": "injected context reminder"}
			]}
		]
	}`)

	chat, err := adapter.Unmarshal(inbound)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
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
				Text      string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}

	// Locate the assistant tool_use turn; the NEXT message must open with
	// tool_result blocks answering both ids, and the trailing text must come
	// only after all results (same or later message).
	assistantIdx := -1
	for i, m := range got.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_use" {
				assistantIdx = i
			}
		}
	}
	if assistantIdx == -1 || assistantIdx+1 >= len(got.Messages) {
		t.Fatalf("no assistant tool_use turn found: %s", out)
	}
	next := got.Messages[assistantIdx+1]
	if next.Role != "user" {
		t.Fatalf("message after tool_use must be the tool-result user turn, got role %s", next.Role)
	}
	var ids []string
	for _, b := range next.Content {
		if b.Type == "text" && len(ids) == 0 {
			t.Fatalf("text precedes tool_results in the reply turn — tool_use/tool_result adjacency broken: %s", out)
		}
		if b.Type == "tool_result" {
			ids = append(ids, b.ToolUseID)
		}
	}
	if len(ids) != 2 || ids[0] != "tu_1" || ids[1] != "tu_2" {
		t.Fatalf("tool results not adjacent to tool_use: got ids %v in message %d\n%s", ids, assistantIdx+1, out)
	}

	// The injected context text must survive the round trip, after the results.
	if !strings.Contains(string(out), "injected context reminder") {
		t.Fatalf("trailing text dropped in round trip: %s", out)
	}
}

// TestSystemStringForm: a bare string `system` is a VALID Anthropic form and
// must canonicalize to exactly ONE system text message with the exact text
// and no cache marker.
func TestSystemStringForm(t *testing.T) {
	adapter := &Adapter{}
	input := `{
		"model": "claude-sonnet-4-20250514",
		"system": "You are a coding agent. Read files and report their contents.",
		"messages": [{"role": "user", "content": "read server.go"}]
	}`
	chat, err := adapter.Unmarshal([]byte(input))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(chat.Messages) != 2 || chat.Messages[0].Role != engine.RoleSystem {
		t.Fatalf("messages = %+v, want system + user", chat.Messages)
	}
	if textOf(chat.Messages[0]) != "You are a coding agent. Read files and report their contents." {
		t.Fatalf("system content = %q", textOf(chat.Messages[0]))
	}
	if hasCacheBlock(chat.Messages[0]) {
		t.Fatalf("string system must not carry a cache marker")
	}
}

// TestSystemStringArrayEquivalence: the string form and the one-text-block
// array form must produce the IDENTICAL canonical IR — and therefore the
// identical marshalled request.
func TestSystemStringArrayEquivalence(t *testing.T) {
	adapter := &Adapter{}
	stringForm := `{
		"model": "m",
		"system": "Be terse.",
		"messages": [{"role": "user", "content": "hi"}]
	}`
	arrayForm := `{
		"model": "m",
		"system": [{"type": "text", "text": "Be terse."}],
		"messages": [{"role": "user", "content": "hi"}]
	}`
	fromString, err := adapter.Unmarshal([]byte(stringForm))
	if err != nil {
		t.Fatalf("unmarshal string form: %v", err)
	}
	fromArray, err := adapter.Unmarshal([]byte(arrayForm))
	if err != nil {
		t.Fatalf("unmarshal array form: %v", err)
	}
	// Same model, same messages.
	if fromString.Model != fromArray.Model || len(fromString.Messages) != len(fromArray.Messages) {
		t.Fatalf("IRs differ: %+v vs %+v", fromString, fromArray)
	}
	for i := range fromString.Messages {
		if fromString.Messages[i].Role != fromArray.Messages[i].Role ||
			textOf(fromString.Messages[i]) != textOf(fromArray.Messages[i]) {
			t.Fatalf("message %d differs: %+v vs %+v", i, fromString.Messages[i], fromArray.Messages[i])
		}
	}
	// Marshal canonicalizes the string form to the array form: byte-identical.
	outS, err := adapter.Marshal(fromString)
	if err != nil {
		t.Fatalf("marshal string-derived: %v", err)
	}
	outA, err := adapter.Marshal(fromArray)
	if err != nil {
		t.Fatalf("marshal array-derived: %v", err)
	}
	if !bytes.Equal(outS, outA) {
		t.Fatalf("marshalled bodies differ:\nstring: %s\narray:  %s", outS, outA)
	}
}

// TestSystemEmptyAndAbsent: `"system": ""` behaves like an absent system
// (the empty canonical block is dropped by the coalescing). An explicit null
// is NOT accepted — the union is string | Array, not nullable; it is covered
// by the invalid-types matrix.
func TestSystemEmptyAndAbsent(t *testing.T) {
	adapter := &Adapter{}
	for _, sys := range []string{`"system": ""`, ``} {
		input := "{\n\t\t\"model\": \"m\",\n\t\t" + sys + `,
		"messages": [{"role": "user", "content": "hi"}]
	}`
		// The no-system variant has no trailing comma problem only when sys is empty.
		if sys == `` {
			input = `{"model": "m", "messages": [{"role": "user", "content": "hi"}]}`
		}
		chat, err := adapter.Unmarshal([]byte(input))
		if err != nil {
			t.Fatalf("unmarshal %s: %v", sys, err)
		}
		for _, m := range chat.Messages {
			if m.Role == engine.RoleSystem {
				t.Fatalf("system %s produced a system message: %+v", sys, chat.Messages)
			}
		}
	}
}

// TestSystemInvalidTypes: a system that is neither a string nor an array is a
// parse error — including an explicit null — and the error never embeds the
// raw body.
func TestSystemInvalidTypes(t *testing.T) {
	adapter := &Adapter{}
	for _, sys := range []string{`5`, `{"type":"text"}`, `true`, `null`} {
		input := `{"model": "m", "system": ` + sys + `, "messages": [{"role": "user", "content": "hi"}]}`
		_, err := adapter.Unmarshal([]byte(input))
		if err == nil {
			t.Fatalf("system %s accepted", sys)
		}
		if strings.Contains(err.Error(), sys) {
			t.Fatalf("error embeds raw body data %s: %v", sys, err)
		}
	}
}

// TestSystemCacheMarkerPreserved: the array form keeps cache-breakpoint
// semantics on the coalesced system message.
func TestSystemCacheMarkerPreserved(t *testing.T) {
	adapter := &Adapter{}
	input := `{
		"model": "m",
		"system": [
			{"type": "text", "text": "prefix"},
			{"type": "text", "text": "suffix", "cache_control": {"type": "ephemeral"}}
		],
		"messages": [{"role": "user", "content": "hi"}]
	}`
	chat, err := adapter.Unmarshal([]byte(input))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if chat.Messages[0].Role != engine.RoleSystem || !hasCacheBlock(chat.Messages[0]) {
		t.Fatalf("cache marker lost: %+v", chat.Messages[0])
	}
	out, err := adapter.Marshal(chat)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"cache_control":{"type":"ephemeral"}`) {
		t.Fatalf("cache marker missing from re-marshalled system: %s", out)
	}
}

// TestSystemArrayTextBlockParam: the system array arm is strictly
// Array<TextBlockParam> — invalid elements are parse errors, never silently
// dropped. Supported member inventory: type, text, cache_control.
func TestSystemArrayTextBlockParam(t *testing.T) {
	adapter := &Adapter{}
	valid := []struct {
		name string
		sys  string
	}{
		{"text block", `[{"type":"text","text":"hi"}]`},
		{"empty text is distinct from missing", `[{"type":"text","text":""}]`},
		{"cache control preserved", `[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]`},
		{"empty array", `[]`},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"model":"m","system":` + tc.sys + `,"messages":[{"role":"user","content":"hi"}]}`
			if _, err := adapter.Unmarshal([]byte(input)); err != nil {
				t.Fatalf("valid system %s rejected: %v", tc.sys, err)
			}
		})
	}
	invalid := []struct {
		name string
		sys  string
	}{
		{"null element", `[null]`},
		{"empty object", `[{}]`},
		{"missing type", `[{"text":"hi"}]`},
		{"non-text type", `[{"type":"tool_use","id":"x","name":"n","input":{}}]`},
		{"missing text", `[{"type":"text"}]`},
		{"text null", `[{"type":"text","text":null}]`},
		{"unknown member dropped", `[{"type":"text","text":"hi","citations":[]}]`},
		{"second element invalid", `[{"type":"text","text":"hi"},null]`},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"model":"m","system":` + tc.sys + `,"messages":[{"role":"user","content":"hi"}]}`
			_, err := adapter.Unmarshal([]byte(input))
			if err == nil {
				t.Fatalf("invalid system accepted: %s", tc.sys)
			}
			if strings.Contains(err.Error(), tc.sys) {
				t.Fatalf("error embeds raw body data: %v", err)
			}
		})
	}
}
