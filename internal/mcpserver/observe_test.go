package mcpserver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func TestObserveCanonicalResponseAcrossShapes(t *testing.T) {
	for _, shape := range []string{"anthropic", "openai-chat", "openai-responses", "gemini", "gemini-codeassist"} {
		t.Run(shape, func(t *testing.T) {
			c := NewCorrelator()
			response := &engine.ChatResponse{ID: "response", UpstreamStatus: 200, FinishReason: "tool_calls", Message: &engine.ResponseMessage{Blocks: []engine.ResponseBlock{{ToolCall: &engine.ResponseToolCall{ID: "call", Name: "mcp__torana__torana_invoke", ArgumentsJSON: []byte(`{"input":{},"operation":"status","namespace":"torana"}`)}}}}}
			if shape == "gemini" || shape == "gemini-codeassist" {
				response.Message.Blocks[0].ToolCall.ID = ""
			}
			now := time.Now()
			if count := c.ObserveResponse(response, shape, "conversation", "host-request", []string{"torana"}, now); count != 1 {
				t.Fatalf("count=%d", count)
			}
			binding, ok := c.Consume("torana_invoke", json.RawMessage(`{"namespace":"torana","operation":"status","input":{}}`), now)
			if !ok || binding.ConversationID != "conversation" || binding.CallID == "" {
				t.Fatalf("binding=%+v ok=%t", binding, ok)
			}
		})
	}
}

func TestObservationRejectsForeignServerAndIncompleteCalls(t *testing.T) {
	for _, tc := range []struct {
		name, finish, id string
		status           int
	}{
		{"mcp__foreign__torana_search", "tool_calls", "call", 200},
		{"mcp__torana__torana_search", "length", "call", 200},
		{"mcp__torana__torana_search", "tool_calls", "call", 500},
		{"mcp__torana__torana_search", "tool_calls", "", 200},
	} {
		c := NewCorrelator()
		response := &engine.ChatResponse{UpstreamStatus: tc.status, FinishReason: tc.finish, Message: &engine.ResponseMessage{Blocks: []engine.ResponseBlock{{ToolCall: &engine.ResponseToolCall{ID: tc.id, Name: tc.name, ArgumentsJSON: []byte(`{}`)}}}}}
		if c.ObserveResponse(response, "anthropic", "conversation", "host-request", []string{"torana"}, time.Now()) != 0 {
			t.Fatalf("unsafe evidence accepted: %+v", tc)
		}
	}
	if responseTool("mcp__custom__torana_search", []string{"custom"}) != "torana_search" {
		t.Fatal("configured server name rejected")
	}
}

func TestGeminiObservationScopesSyntheticIDsToHostRequest(t *testing.T) {
	c := NewCorrelator()
	now := time.Now()
	for _, query := range []string{"first", "second"} {
		response := &engine.ChatResponse{UpstreamStatus: 200, FinishReason: "STOP", Message: &engine.ResponseMessage{Blocks: []engine.ResponseBlock{{ToolCall: &engine.ResponseToolCall{ID: "torana_search", Name: "torana_search", ArgumentsJSON: []byte(`{"query":"` + query + `"}`)}}}}}
		if c.ObserveResponse(response, "gemini", "conversation", "request-"+query, nil, now) != 1 {
			t.Fatal("native synthetic ID not recorded")
		}
	}
	for _, query := range []string{"first", "second"} {
		binding, ok := c.Consume("torana_search", json.RawMessage(`{"query":"`+query+`"}`), now)
		if !ok || binding.CallID != "gemini:request-"+query+":0" {
			t.Fatalf("binding=%+v ok=%t", binding, ok)
		}
	}
}
