package proxy

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/mcpserver"
)

func TestMCPObservationRealResponseExtractors(t *testing.T) {
	for _, tc := range []struct{ shape, format, wire string }{
		{"anthropic", "anthropic", `{"id":"msg","stop_reason":"tool_use","content":[{"type":"tool_use","id":"call","name":"torana_search","input":{"query":"status"}}]}`},
		{"openai-chat", "openai", `{"id":"chat","choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"call","function":{"name":"torana_search","arguments":"{\"query\":\"status\"}"}}]}}]}`},
		{"openai-responses", "openai", `{"id":"resp","status":"completed","output":[{"type":"function_call","call_id":"call","name":"torana_search","arguments":"{\"query\":\"status\"}"}]}`},
		{"gemini", "gemini", `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"torana_search","args":{"query":"status"}}}]}}]}`},
		{"gemini-codeassist", "gemini-codeassist", `{"response":{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"torana_search","args":{"query":"status"}}}]}}]}}`},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal([]byte(tc.wire), &body); err != nil {
				t.Fatal(err)
			}
			refs := extractResponse(tc.format, body, []byte(tc.wire))
			response := &engine.ChatResponse{ID: refs.id, Message: refs.assistantMessage(), FinishReason: refs.finishReason, UpstreamStatus: 200}
			c := mcpserver.NewCorrelator()
			if count := c.ObserveResponse(response, tc.shape, "conversation", "request-1", []string{"torana"}, time.Now()); count != 1 {
				t.Fatalf("extractor evidence not recorded: count=%d finish=%q", count, refs.finishReason)
			}
			if binding, ok := c.Consume("torana_search", json.RawMessage(`{"query":"status"}`), time.Now()); !ok || binding.ConversationID != "conversation" {
				t.Fatalf("binding=%+v ok=%t", binding, ok)
			}
		})
	}
}

func TestResponsesCompletionStatusForObservation(t *testing.T) {
	for _, status := range []string{"incomplete", "failed", "in_progress", ""} {
		wire := `{"status":"` + status + `","output":[{"type":"function_call","call_id":"call","name":"torana_search","arguments":"{}"}]}`
		var body map[string]any
		if err := json.Unmarshal([]byte(wire), &body); err != nil {
			t.Fatal(err)
		}
		refs := extractResponse("openai", body, []byte(wire))
		c := mcpserver.NewCorrelator()
		if c.ObserveResponse(&engine.ChatResponse{Message: refs.assistantMessage(), FinishReason: refs.finishReason, UpstreamStatus: 200}, "openai-responses", "conversation", "request-1", nil, time.Now()) != 0 {
			t.Fatalf("non-completed status %q recorded", status)
		}
	}
}
