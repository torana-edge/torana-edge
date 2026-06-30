package vertex

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func TestRoundTrip(t *testing.T) {
	a := &Adapter{}

	// Sample Gemini request with system instruction, text, and a function call.
	input := `{
		"systemInstruction": {
			"parts": [{"text": "You are a helpful assistant."}]
		},
		"contents": [
			{"role": "user", "parts": [{"text": "What is the weather in Paris?"}]},
			{"role": "model", "parts": [{"functionCall": {"name": "get_weather", "args": {"location": "Paris", "unit": "celsius"}}}]},
			{"role": "user", "parts": [{"functionResponse": {"name": "get_weather", "response": {"temperature": 22, "condition": "sunny"}}}]},
			{"role": "model", "parts": [{"text": "The weather in Paris is 22C and sunny."}]}
		],
		"tools": [{"functionDeclarations": [{"name": "get_weather", "description": "Get current weather", "parameters": {"type": "object", "properties": {"location": {"type": "string"}}}}]}]
	}`

	chat, err := a.Unmarshal(&http.Request{}, []byte(input))
	if err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if len(chat.Messages) != 5 {
		t.Fatalf("Expected 5 messages, got %d", len(chat.Messages))
	}

	if chat.Messages[0].Role != engine.RoleSystem {
		t.Errorf("Message 0: expected system, got %s", chat.Messages[0].Role)
	}
	if chat.Messages[0].Content != "You are a helpful assistant." {
		t.Errorf("Message 0: wrong content: %s", chat.Messages[0].Content)
	}

	if chat.Messages[1].Role != engine.RoleUser {
		t.Errorf("Message 1: expected user, got %s", chat.Messages[1].Role)
	}

	if len(chat.Messages[2].ToolCalls) != 1 {
		t.Fatalf("Message 2: expected 1 tool call, got %d", len(chat.Messages[2].ToolCalls))
	}
	tc := chat.Messages[2].ToolCalls[0]
	if tc.Name != "get_weather" {
		t.Errorf("ToolCall name: expected get_weather, got %s", tc.Name)
	}
	if tc.Arguments["location"] != "Paris" {
		t.Errorf("ToolCall args: location = %v", tc.Arguments["location"])
	}

	if len(chat.Tools) != 1 {
		t.Fatalf("Expected 1 tool, got %d", len(chat.Tools))
	}
	if chat.Tools[0].Name != "get_weather" {
		t.Errorf("Tool name: %s", chat.Tools[0].Name)
	}

	// Marshal back.
	output, err := a.Marshal(chat)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("Marshal output is not valid JSON: %v\noutput: %s", err, string(output))
	}

	// Verify round-trip produces valid structure.
	contents, ok := result["contents"].([]any)
	if !ok {
		t.Fatalf("Marshal output missing contents array")
	}
	if len(contents) < 3 {
		t.Fatalf("Expected at least 3 contents, got %d", len(contents))
	}
}

func TestUnmarshalNoSystem(t *testing.T) {
	a := &Adapter{}
	input := `{"contents": [{"role": "user", "parts": [{"text": "Hello"}]}]}`

	chat, err := a.Unmarshal(&http.Request{}, []byte(input))
	if err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if len(chat.Messages) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(chat.Messages))
	}
	if chat.Messages[0].Role != engine.RoleUser {
		t.Errorf("Expected user role, got %s", chat.Messages[0].Role)
	}
}

func TestStreamParse(t *testing.T) {
	s := &StreamAdapter{}

	lines := `{"candidates": [{"content": {"parts": [{"text": "Let me check the weather."}], "role": "model"}}]}
{"candidates": [{"content": {"parts": [{"functionCall": {"name": "get_weather", "args": {"location": "Paris"}}}], "role": "model"}}]}
{"candidates": [{"finishReason": "STOP"}]}
`
	r := strings.NewReader(lines)
	ch := s.ParseStream(r)

	var events []engine.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}

	if len(events) != 5 {
		t.Fatalf("Expected 5 events, got %d: %+v", len(events), events)
	}

	if events[0].TextDelta == nil || *events[0].TextDelta != "Let me check the weather." {
		t.Errorf("Event 0: expected TextDelta, got %+v", events[0])
	}

	if events[1].ToolCallStart == nil || events[1].ToolCallStart.Name != "get_weather" {
		t.Errorf("Event 1: expected ToolCallStart get_weather, got %+v", events[1])
	}

	if events[2].ToolCallDelta == nil {
		t.Errorf("Event 2: expected ToolCallDelta, got %+v", events[2])
	} else {
		delta := events[2].ToolCallDelta.ArgumentsDelta
		var args map[string]any
		if err := json.Unmarshal([]byte(delta), &args); err != nil {
			t.Errorf("ToolCallDelta args not valid JSON: %s (error: %v)", delta, err)
		}
		if args["location"] != "Paris" {
			t.Errorf("ToolCallDelta args: location = %v", args["location"])
		}
	}

	if events[3].ToolCallEnd == nil {
		t.Errorf("Event 3: expected ToolCallEnd, got %+v", events[3])
	}

	if events[4].FinishReason != "stop" {
		t.Errorf("Event 4: expected FinishReason 'stop', got %s", events[4].FinishReason)
	}
}

func TestStreamSerialize(t *testing.T) {
	s := &StreamAdapter{}

	events := make(chan engine.StreamEvent, 5)
	text := "Hello"
	events <- engine.StreamEvent{TextDelta: &text}
	events <- engine.StreamEvent{
		ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "get_weather", Name: "get_weather"},
	}
	events <- engine.StreamEvent{
		ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{"location":"Paris"}`},
	}
	events <- engine.StreamEvent{
		ToolCallEnd: &engine.ToolCallEnd{Index: 0},
	}
	events <- engine.StreamEvent{FinishReason: "stop"}
	close(events)

	var buf strings.Builder
	if err := s.SerializeStream(&buf, events); err != nil {
		t.Fatalf("SerializeStream error: %v", err)
	}

	output := buf.String()
	rawLines := strings.Split(strings.TrimSpace(output), "\n")
	if len(rawLines) < 3 {
		t.Fatalf("Expected at least 3 lines, got %d:\n%s", len(rawLines), output)
	}

	// First line should have text
	var line1 geminiStreamChunk
	json.Unmarshal([]byte(rawLines[0]), &line1)
	if len(line1.Candidates) == 0 || line1.Candidates[0].Content == nil {
		t.Fatalf("Line 1 missing content")
	}
	if len(line1.Candidates[0].Content.Parts) == 0 || line1.Candidates[0].Content.Parts[0].Text != "Hello" {
		t.Errorf("Line 1 text mismatch")
	}

	// Second line should have functionCall
	var line2 geminiStreamChunk
	json.Unmarshal([]byte(rawLines[1]), &line2)
	if len(line2.Candidates) == 0 || line2.Candidates[0].Content == nil {
		t.Fatalf("Line 2 missing content")
	}
	if len(line2.Candidates[0].Content.Parts) == 0 || line2.Candidates[0].Content.Parts[0].FunctionCall == nil {
		t.Fatalf("Line 2 missing functionCall")
	}
	fc := line2.Candidates[0].Content.Parts[0].FunctionCall
	if fc.Name != "get_weather" {
		t.Errorf("Line 2 functionCall name: %s", fc.Name)
	}

	// Last line should have finishReason
	var lineN geminiStreamChunk
	json.Unmarshal([]byte(rawLines[len(rawLines)-1]), &lineN)
	if len(lineN.Candidates) == 0 || lineN.Candidates[0].FinishReason != "STOP" {
		t.Errorf("Last line: expected finishReason STOP, got %+v", lineN)
	}
}
