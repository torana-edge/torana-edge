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

func TestTranscriptInvocationsUseOnlyLatestCompletedExchange(t *testing.T) {
	args, err := engine.ParseRequiredJSONObject([]byte(`{"namespace":"logger","operation":"_disable"}`))
	if err != nil {
		t.Fatal(err)
	}
	request := &engine.ChatRequest{Messages: []engine.Message{
		{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{ID: "old", Name: "mcp__torana__torana_invoke", Arguments: args}}}},
		{Role: engine.RoleUser, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{ToolCallID: "old", Content: []engine.ToolResultContentBlock{{Text: `{"ok":true,"status":"pending","ticket":"old-ticket"}`}}}}}},
		{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{ID: "new", Name: "mcp__torana__torana_invoke", Arguments: args}}}},
		{Role: engine.RoleUser, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{ToolCallID: "new", Content: []engine.ToolResultContentBlock{{Text: `{"ok":true,"status":"pending","ticket":"new-ticket"}`}}}}}},
	}}
	calls := TranscriptInvocations(request, []string{"torana"})
	if len(calls) != 1 || calls[0].CallID != "new" || calls[0].Ticket != "new-ticket" || string(calls[0].Input) != string(args.Bytes()) {
		t.Fatalf("calls=%+v", calls)
	}
	request.Messages = append(request.Messages, engine.Message{Role: engine.RoleAssistant, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "done"}}}})
	if calls := TranscriptInvocations(request, []string{"torana"}); len(calls) != 0 {
		t.Fatalf("old exchange replayed: %+v", calls)
	}
}

func TestTranscriptInvocationsRequireSuccessfulToranaTicket(t *testing.T) {
	args, _ := engine.ParseRequiredJSONObject([]byte(`{"namespace":"logger","operation":"_disable"}`))
	failed := true
	for _, result := range []*engine.ToolResultBlock{
		{ToolCallID: "call", IsError: &failed, Content: []engine.ToolResultContentBlock{{Text: `{"ok":true,"status":"pending","ticket":"ticket"}`}}},
		{ToolCallID: "call", Content: []engine.ToolResultContentBlock{{Text: "user denied this tool call"}}},
		{ToolCallID: "call", Content: []engine.ToolResultContentBlock{{Text: `{"ok":true,"status":"pending"}`}}},
	} {
		request := &engine.ChatRequest{Messages: []engine.Message{
			{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{ID: "call", Name: "mcp__torana__torana_invoke", Arguments: args}}}},
			{Role: engine.RoleUser, Blocks: []engine.Block{{ToolResult: result}}},
		}}
		if calls := TranscriptInvocations(request, []string{"torana"}); len(calls) != 0 {
			t.Fatalf("unsafe result accepted: %+v", calls)
		}
	}
}

func TestTranscriptInvocationsRejectForeignIncompleteAndConflictingCalls(t *testing.T) {
	first, _ := engine.ParseRequiredJSONObject([]byte(`{"namespace":"logger","operation":"_disable"}`))
	second, _ := engine.ParseRequiredJSONObject([]byte(`{"namespace":"logger","operation":"_enable"}`))
	request := &engine.ChatRequest{Messages: []engine.Message{
		{Role: engine.RoleAssistant, Blocks: []engine.Block{
			{ToolUse: &engine.ToolUseBlock{ID: "conflict", Name: "mcp__torana__torana_invoke", Arguments: first}},
			{ToolUse: &engine.ToolUseBlock{ID: "conflict", Name: "mcp__torana__torana_invoke", Arguments: second}},
			{ToolUse: &engine.ToolUseBlock{ID: "foreign", Name: "mcp__foreign__torana_invoke", Arguments: first}},
			{ToolUse: &engine.ToolUseBlock{ID: "unfinished", Name: "mcp__torana__torana_invoke", Arguments: first}},
		}},
		{Role: engine.RoleUser, Blocks: []engine.Block{
			{ToolResult: &engine.ToolResultBlock{ToolCallID: "conflict", Content: []engine.ToolResultContentBlock{{Text: "x"}}}},
			{ToolResult: &engine.ToolResultBlock{ToolCallID: "foreign", Content: []engine.ToolResultContentBlock{{Text: "x"}}}},
		}},
	}}
	if calls := TranscriptInvocations(request, []string{"torana"}); len(calls) != 0 {
		t.Fatalf("unsafe calls accepted: %+v", calls)
	}
}
