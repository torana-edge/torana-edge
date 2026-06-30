package engine

import (
	"errors"
	"net/http"
	"testing"
)

// --- test helpers ---

// testRequestHook is a configurable RequestHook for testing.
type testRequestHook struct {
	name string
	fn   func(req *http.Request, chat *ChatRequest) (*ChatRequest, error)
}

func (h *testRequestHook) Name() string { return h.name }

func (h *testRequestHook) BeforeRequest(req *http.Request, chat *ChatRequest) (*ChatRequest, error) {
	return h.fn(req, chat)
}

// testResponseHook is a configurable ResponseHook for testing.
type testResponseHook struct {
	name string
	fn   func(resp *http.Response, events <-chan StreamEvent, req *http.Request, chat *ChatRequest) (<-chan StreamEvent, error)
}

func (h *testResponseHook) Name() string { return h.name }

func (h *testResponseHook) AfterResponse(resp *http.Response, events <-chan StreamEvent,
	req *http.Request, chat *ChatRequest) (<-chan StreamEvent, error) {
	return h.fn(resp, events, req, chat)
}

// --- RequestHook tests ---

func TestRunBeforeRequest_ChainsHooks(t *testing.T) {
	p := New()

	p.AddRequestHook(&testRequestHook{
		name: "add-system",
		fn: func(req *http.Request, chat *ChatRequest) (*ChatRequest, error) {
			chat.Messages = append(chat.Messages, Message{Role: RoleSystem, Content: "system-msg"})
			return chat, nil
		},
	})
	p.AddRequestHook(&testRequestHook{
		name: "add-tool",
		fn: func(req *http.Request, chat *ChatRequest) (*ChatRequest, error) {
			chat.Tools = append(chat.Tools, ToolDef{Name: "test-tool", Description: "a test tool"})
			return chat, nil
		},
	})

	req := &http.Request{}
	chat := &ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	}

	result := p.RunBeforeRequest(req, chat)

	if len(result.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result.Messages))
	}
	if result.Messages[1].Role != RoleSystem || result.Messages[1].Content != "system-msg" {
		t.Errorf("system message mismatch: %+v", result.Messages[1])
	}
	if len(result.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result.Tools))
	}
	if result.Tools[0].Name != "test-tool" {
		t.Errorf("tool name mismatch: %s", result.Tools[0].Name)
	}
}

func TestRunBeforeRequest_ErrorContinues(t *testing.T) {
	p := New()

	p.AddRequestHook(&testRequestHook{
		name: "error-hook",
		fn: func(req *http.Request, chat *ChatRequest) (*ChatRequest, error) {
			return nil, errors.New("boom")
		},
	})
	p.AddRequestHook(&testRequestHook{
		name: "success-hook",
		fn: func(req *http.Request, chat *ChatRequest) (*ChatRequest, error) {
			chat.Messages = append(chat.Messages, Message{Role: RoleAssistant, Content: "still-ran"})
			return chat, nil
		},
	})

	req := &http.Request{}
	chat := &ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	}

	result := p.RunBeforeRequest(req, chat)

	if len(result.Messages) != 2 {
		t.Fatalf("expected 2 messages (original + success-hook), got %d", len(result.Messages))
	}
	if result.Messages[1].Role != RoleAssistant || result.Messages[1].Content != "still-ran" {
		t.Errorf("success hook did not run: %+v", result.Messages[1])
	}
}

func TestRunBeforeRequest_NilReturnPassthrough(t *testing.T) {
	p := New()

	p.AddRequestHook(&testRequestHook{
		name: "nil-return",
		fn: func(req *http.Request, chat *ChatRequest) (*ChatRequest, error) {
			return nil, nil
		},
	})
	p.AddRequestHook(&testRequestHook{
		name: "modifier",
		fn: func(req *http.Request, chat *ChatRequest) (*ChatRequest, error) {
			chat.Messages = append(chat.Messages, Message{Role: RoleSystem, Content: "modifier-ran"})
			return chat, nil
		},
	})

	req := &http.Request{}
	chat := &ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	}

	result := p.RunBeforeRequest(req, chat)

	if len(result.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result.Messages))
	}
	if result.Messages[0].Role != RoleUser || result.Messages[0].Content != "hello" {
		t.Errorf("original message lost: %+v", result.Messages[0])
	}
	if result.Messages[1].Role != RoleSystem || result.Messages[1].Content != "modifier-ran" {
		t.Errorf("modifier hook did not run: %+v", result.Messages[1])
	}
}

// --- ResponseHook tests ---

func TestRunAfterResponse_ChainsHooks(t *testing.T) {
	p := New()

	p.AddResponseHook(&testResponseHook{
		name: "hook1",
		fn: func(resp *http.Response, in <-chan StreamEvent, req *http.Request, chat *ChatRequest) (<-chan StreamEvent, error) {
			out := make(chan StreamEvent, 4)
			go func() {
				defer close(out)
				msg := "from-hook1"
				first := true
				for ev := range in {
					out <- ev
					if first {
						out <- StreamEvent{TextDelta: &msg}
						first = false
					}
				}
			}()
			return out, nil
		},
	})
	p.AddResponseHook(&testResponseHook{
		name: "hook2",
		fn: func(resp *http.Response, in <-chan StreamEvent, req *http.Request, chat *ChatRequest) (<-chan StreamEvent, error) {
			out := make(chan StreamEvent, 4)
			go func() {
				defer close(out)
				for ev := range in {
					out <- ev
				}
				msg := "from-hook2"
				out <- StreamEvent{TextDelta: &msg}
			}()
			return out, nil
		},
	})

	// Feed one event into the pipeline.
	in := make(chan StreamEvent, 1)
	msg := "hello"
	in <- StreamEvent{TextDelta: &msg}
	close(in)

	resp := &http.Response{}
	req := &http.Request{}
	chat := &ChatRequest{}

	out := p.RunAfterResponse(resp, in, req, chat)

	var events []StreamEvent
	for ev := range out {
		events = append(events, ev)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0].TextDelta == nil || *events[0].TextDelta != "hello" {
		t.Errorf("event 0 mismatch: %+v", events[0])
	}
	if events[1].TextDelta == nil || *events[1].TextDelta != "from-hook1" {
		t.Errorf("event 1 mismatch: %+v", events[1])
	}
	if events[2].TextDelta == nil || *events[2].TextDelta != "from-hook2" {
		t.Errorf("event 2 mismatch: %+v", events[2])
	}
}

func TestRunAfterResponse_ErrorContinues(t *testing.T) {
	p := New()

	p.AddResponseHook(&testResponseHook{
		name: "error-hook",
		fn: func(resp *http.Response, in <-chan StreamEvent, req *http.Request, chat *ChatRequest) (<-chan StreamEvent, error) {
			return nil, errors.New("boom")
		},
	})
	p.AddResponseHook(&testResponseHook{
		name: "success-hook",
		fn: func(resp *http.Response, in <-chan StreamEvent, req *http.Request, chat *ChatRequest) (<-chan StreamEvent, error) {
			out := make(chan StreamEvent, 2)
			go func() {
				defer close(out)
				for ev := range in {
					out <- ev
				}
				msg := "success-hook-ran"
				out <- StreamEvent{TextDelta: &msg}
			}()
			return out, nil
		},
	})

	in := make(chan StreamEvent, 1)
	msg := "original"
	in <- StreamEvent{TextDelta: &msg}
	close(in)

	resp := &http.Response{}
	req := &http.Request{}
	chat := &ChatRequest{}

	out := p.RunAfterResponse(resp, in, req, chat)

	var events []StreamEvent
	for ev := range out {
		events = append(events, ev)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events (original + success-hook), got %d", len(events))
	}
	if events[0].TextDelta == nil || *events[0].TextDelta != "original" {
		t.Errorf("event 0 mismatch: %+v", events[0])
	}
	if events[1].TextDelta == nil || *events[1].TextDelta != "success-hook-ran" {
		t.Errorf("event 1 mismatch: %+v", events[1])
	}
}
