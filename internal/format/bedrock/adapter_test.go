package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func TestRoundTrip(t *testing.T) {
	adapter := &Adapter{}

	input := `{
		"modelId": "anthropic.claude-sonnet-4-20250514-v1:0",
		"system": [{"text": "You are helpful."}],
		"messages": [
			{"role": "user", "content": [{"text": "What's the weather?"}]},
			{"role": "assistant", "content": [
				{"toolUse": {"toolUseId": "toolu_1", "name": "get_weather", "input": {"city": "SF"}}}
			]},
			{"role": "user", "content": [
				{"toolResult": {"toolUseId": "toolu_1", "content": [{"text": "Sunny, 72F"}]}}
			]}
		],
		"toolConfig": {
			"tools": [{
				"toolSpec": {
					"name": "get_weather",
					"description": "Get weather",
					"inputSchema": {
						"json": {
							"type": "object",
							"properties": {
								"city": {"type": "string"}
							}
						}
					}
				}
			}]
		}
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
		t.Errorf("msg 0 content: got %q, want 'You are helpful.'", chat.Messages[0].Content)
	}

	if chat.Messages[1].Role != engine.RoleUser {
		t.Errorf("msg 1 role: got %s, want user", chat.Messages[1].Role)
	}
	if chat.Messages[1].Content != "What's the weather?" {
		t.Errorf("msg 1 content: got %q", chat.Messages[1].Content)
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
	if chat.Messages[2].ToolCalls[0].ID != "toolu_1" {
		t.Errorf("tool call id: got %s", chat.Messages[2].ToolCalls[0].ID)
	}
	city, ok := chat.Messages[2].ToolCalls[0].Arguments["city"].(string)
	if !ok || city != "SF" {
		t.Errorf("tool call args: got %v", chat.Messages[2].ToolCalls[0].Arguments)
	}

	if chat.Messages[3].Role != engine.RoleTool {
		t.Errorf("msg 3 role: got %s, want tool", chat.Messages[3].Role)
	}
	if chat.Messages[3].ToolCallID != "toolu_1" {
		t.Errorf("msg 3 tool_call_id: got %s", chat.Messages[3].ToolCallID)
	}
	if chat.Messages[3].Content != "Sunny, 72F" {
		t.Errorf("msg 3 content: got %q", chat.Messages[3].Content)
	}

	// Verify tools — Parameters are nested under inputSchema.json
	if len(chat.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(chat.Tools))
	}
	if chat.Tools[0].Name != "get_weather" {
		t.Errorf("tool name: got %s", chat.Tools[0].Name)
	}
	if chat.Tools[0].Description != "Get weather" {
		t.Errorf("tool description: got %s", chat.Tools[0].Description)
	}
	if chat.Tools[0].Parameters == nil {
		t.Fatal("tool parameters are nil")
	}
	paramsType, ok := chat.Tools[0].Parameters["type"].(string)
	if !ok || paramsType != "object" {
		t.Errorf("tool parameters type: got %v", chat.Tools[0].Parameters)
	}

	// Marshal back
	output, err := adapter.Marshal(chat)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(output, &out); err != nil {
		t.Fatalf("marshal output not valid JSON: %v\n%s", err, output)
	}

	// Verify messages array
	msgs, ok := out["messages"].([]any)
	if !ok || len(msgs) < 3 {
		t.Fatalf("output messages: got %v", out["messages"])
	}

	// Verify toolConfig structure — Parameters nested under toolSpec.inputSchema.json
	toolConfig, ok := out["toolConfig"].(map[string]any)
	if !ok {
		t.Fatal("output missing toolConfig")
	}
	tools, ok := toolConfig["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatal("output toolConfig.tools missing or empty")
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatal("output toolConfig.tools[0] not an object")
	}
	toolSpec, ok := tool["toolSpec"].(map[string]any)
	if !ok {
		t.Fatal("output toolConfig.tools[0].toolSpec missing")
	}
	if toolSpec["name"] != "get_weather" {
		t.Errorf("marshaled tool name: got %v", toolSpec["name"])
	}
	inputSchema, ok := toolSpec["inputSchema"].(map[string]any)
	if !ok {
		t.Fatal("output toolSpec.inputSchema missing")
	}
	jsonParams, ok := inputSchema["json"].(map[string]any)
	if !ok {
		t.Fatal("output inputSchema.json missing — Parameters not nested correctly")
	}
	if jsonParams["type"] != "object" {
		t.Errorf("marshaled params: got %v", jsonParams)
	}

	// Verify system block
	systemVal, hasSystem := out["system"]
	if !hasSystem {
		t.Fatal("output missing system")
	}
	sysBlocks, ok := systemVal.([]any)
	if !ok || len(sysBlocks) == 0 {
		t.Fatal("output system not an array")
	}
	sysBlock, ok := sysBlocks[0].(map[string]any)
	if !ok || sysBlock["text"] != "You are helpful." {
		t.Errorf("output system block: got %v", sysBlocks)
	}
}

func TestStreamParse(t *testing.T) {
	stream := &Stream{}

	// Bedrock ConverseStream raw JSON lines (not SSE data: prefix)
	input := `{"messageStart":{"role":"assistant"}}
{"contentBlockStart":{"contentBlockIndex":0,"start":{"text":""}}}
{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"Hello"}}}
{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":" world"}}}
{"contentBlockStop":{"contentBlockIndex":0}}
{"contentBlockStart":{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"toolu_1","name":"get_weather"}}}}
{"contentBlockDelta":{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"city\":\"SF\"}"}}}}
{"contentBlockStop":{"contentBlockIndex":1}}
{"messageStop":{"stopReason":"tool_use"}}
`

	ch := stream.ParseStream(io.NopCloser(strings.NewReader(input)))

	events := make([]engine.StreamEvent, 0)
	for evt := range ch {
		events = append(events, evt)
	}

	// Expected: TextDelta("Hello"), TextDelta(" world"),
	//   ToolCallStart, ToolCallDelta, ToolCallEnd, FinishReason("tool_calls")
	if len(events) != 6 {
		t.Fatalf("expected 6 events, got %d: %+v", len(events), events)
	}

	// Text deltas
	if events[0].TextDelta == nil || *events[0].TextDelta != "Hello" {
		t.Errorf("event 0: expected TextDelta 'Hello', got %+v", events[0])
	}
	if events[1].TextDelta == nil || *events[1].TextDelta != " world" {
		t.Errorf("event 1: expected TextDelta ' world', got %+v", events[1])
	}

	// ToolCallStart (index 0 for bedrock, since only one tool call per turn)
	if events[2].ToolCallStart == nil {
		t.Fatalf("event 2: expected ToolCallStart, got %+v", events[2])
	}
	tcs := events[2].ToolCallStart
	if tcs.Index != 0 || tcs.ID != "toolu_1" || tcs.Name != "get_weather" {
		t.Errorf("ToolCallStart: got {idx:%d id:%s name:%s}", tcs.Index, tcs.ID, tcs.Name)
	}

	// ToolCallDelta
	if events[3].ToolCallDelta == nil {
		t.Fatalf("event 3: expected ToolCallDelta, got %+v", events[3])
	}
	if events[3].ToolCallDelta.ArgumentsDelta != `{"city":"SF"}` {
		t.Errorf("ToolCallDelta: got %q", events[3].ToolCallDelta.ArgumentsDelta)
	}

	// ToolCallEnd from tool block stop
	if events[4].ToolCallEnd == nil {
		t.Errorf("event 4: expected ToolCallEnd, got %+v", events[4])
	}

	// Message stop
	if events[5].FinishReason != "tool_calls" {
		t.Errorf("event 5: expected FinishReason tool_calls, got %+v", events[5])
	}
}

func TestStreamSerialize(t *testing.T) {
	stream := &Stream{}

	hello := "Hello"
	world := " world"

	events := []engine.StreamEvent{
		{TextDelta: &hello},
		{TextDelta: &world},
		{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "toolu_1", Name: "get_weather"}},
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
	if err := stream.SerializeStream(context.Background(), &buf, evtCh); err != nil {
		t.Fatalf("serialize: %v", err)
	}

	output := buf.String()
	t.Logf("serialized:\n%s", output)

	// Parse back
	parsedCh := stream.ParseStream(io.NopCloser(bytes.NewReader([]byte(output))))
	parsed := make([]engine.StreamEvent, 0)
	for evt := range parsedCh {
		parsed = append(parsed, evt)
	}

	// The Bedrock serializer emits all events including contentBlockStop for ToolCallEnd,
	// so the round-trip should produce the same 6 events.
	if len(parsed) != len(events) {
		t.Fatalf("round-trip: expected %d events, got %d", len(events), len(parsed))
	}

	if parsed[0].TextDelta == nil || *parsed[0].TextDelta != "Hello" {
		t.Errorf("rt event 0: got %+v", parsed[0])
	}
	if parsed[1].TextDelta == nil || *parsed[1].TextDelta != " world" {
		t.Errorf("rt event 1: got %+v", parsed[1])
	}
	if parsed[2].ToolCallStart == nil || parsed[2].ToolCallStart.Name != "get_weather" {
		t.Errorf("rt event 2: got %+v", parsed[2])
	}
	if parsed[3].ToolCallDelta == nil || parsed[3].ToolCallDelta.ArgumentsDelta != `{"city":"SF"}` {
		t.Errorf("rt event 3: got %+v", parsed[3])
	}
	if parsed[4].ToolCallEnd == nil {
		t.Errorf("rt event 4: got %+v", parsed[4])
	}
	if parsed[5].FinishReason != "tool_calls" {
		t.Errorf("rt event 5: got %+v", parsed[5])
	}
}

func TestStreamParse_EndTurnFinish(t *testing.T) {
	stream := &Stream{}

	// end_turn maps to "stop"
	input := `{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"Done"}}}
{"messageStop":{"stopReason":"end_turn"}}
`

	ch := stream.ParseStream(io.NopCloser(strings.NewReader(input)))
	events := make([]engine.StreamEvent, 0)
	for evt := range ch {
		events = append(events, evt)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}
	if events[0].TextDelta == nil || *events[0].TextDelta != "Done" {
		t.Errorf("event 0: got %+v", events[0])
	}
	if events[1].FinishReason != "stop" {
		t.Errorf("event 1: expected FinishReason 'stop', got %q", events[1].FinishReason)
	}
}
