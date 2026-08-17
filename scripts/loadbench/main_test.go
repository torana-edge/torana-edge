package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestPayloadShapes(t *testing.T) {
	type function struct {
		Name       string          `json:"name"`
		Arguments  string          `json:"arguments"`
		Parameters json.RawMessage `json:"parameters"`
	}
	type toolCall struct {
		ID       string   `json:"id"`
		Type     string   `json:"type"`
		Function function `json:"function"`
	}
	type message struct {
		Role       string     `json:"role"`
		Content    string     `json:"content"`
		ToolCallID string     `json:"tool_call_id"`
		ToolCalls  []toolCall `json:"tool_calls"`
	}
	type request struct {
		Model    string `json:"model"`
		Messages []message
		Tools    []struct {
			Type     string   `json:"type"`
			Function function `json:"function"`
		} `json:"tools"`
		Stream bool `json:"stream"`
	}

	plain, err := requestPayload("plain", 7, true)
	if err != nil {
		t.Fatal(err)
	}
	var p request
	if err := json.Unmarshal(plain, &p); err != nil {
		t.Fatal(err)
	}
	if p.Model != "gpt-bench" || !p.Stream || len(p.Messages) != 1 || p.Messages[0].Role != "user" || p.Messages[0].Content != "ppppppp" || len(p.Tools) != 0 {
		t.Fatalf("plain request = %+v", p)
	}

	agent, err := requestPayload("agent", 11, false)
	if err != nil {
		t.Fatal(err)
	}
	var a request
	if err := json.Unmarshal(agent, &a); err != nil {
		t.Fatal(err)
	}
	if a.Stream || len(a.Messages) != 6 || len(a.Tools) != 1 {
		t.Fatalf("agent cardinality = stream=%v messages=%d tools=%d", a.Stream, len(a.Messages), len(a.Tools))
	}
	call := a.Messages[2].ToolCalls
	if a.Messages[2].Role != "assistant" || len(call) != 1 || call[0].ID != "call_bench" || call[0].Type != "function" || call[0].Function.Name != "read_file" || call[0].Function.Arguments != `{"path":"internal/parser.go"}` {
		t.Fatalf("agent tool call = %+v", a.Messages[2])
	}
	if a.Messages[3].Role != "tool" || a.Messages[3].ToolCallID != "call_bench" || a.Messages[3].Content != strings.Repeat("p", 11) {
		t.Fatalf("agent tool result = %+v", a.Messages[3])
	}
	if a.Tools[0].Type != "function" || a.Tools[0].Function.Name != "read_file" || !strings.Contains(string(a.Tools[0].Function.Parameters), `"additionalProperties":{"type":"string"}`) {
		t.Fatalf("agent tool = %+v", a.Tools[0])
	}

	if _, err := requestPayload("unknown", 1, false); err == nil {
		t.Fatal("unknown request shape accepted")
	}
}

func TestOneRequestValidatesCompleteStream(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
		ok   bool
	}{
		{"complete", "data: {\"x\":1}\n\ndata: {\"x\":2}\n\ndata: [DONE]\n\n", 2, true},
		{"missing done", "data: {\"x\":1}\n\n", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			got, err := oneRequest(srv.Client(), srv.URL, []byte(`{}`), true)
			if (err == nil) != tc.ok || got != tc.want {
				t.Fatalf("oneRequest = (%d, %v), want (%d, ok=%v)", got, err, tc.want, tc.ok)
			}
		})
	}
}

func TestPercentileUsesStableIndex(t *testing.T) {
	values := []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond, 4 * time.Millisecond}
	if got := percentile(values, 0.50); got != 2*time.Millisecond {
		t.Fatalf("p50 = %s", got)
	}
	if got := percentile(values, 0.99); got != 4*time.Millisecond {
		t.Fatalf("p99 = %s", got)
	}
}

func TestParseProcessCPUTicksUsesFieldsAfterFinalCommandParen(t *testing.T) {
	stat := "42 (torana bench) S 1 2 3 4 5 6 7 8 9 10 120 30 16"
	got, err := parseProcessCPUTicks(stat)
	if err != nil {
		t.Fatal(err)
	}
	if got != 150 {
		t.Fatalf("ticks = %d, want 150", got)
	}
}
