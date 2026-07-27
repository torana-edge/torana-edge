package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// The non-streaming reader understood only the Chat Completions shape. A
// Responses API reply therefore produced no usage (accounted at zero tokens and
// zero cost) and no content references (invisible to response hooks) — both
// silently, because an absent field looks exactly like a provider that reported
// nothing. The streaming path had understood the shape all along, so the two
// disagreed based only on whether the client asked for a stream.

func decode(t *testing.T, raw string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return body
}

const responsesReply = `{
  "id": "resp_1",
  "object": "response",
  "model": "gpt-4.1",
  "output": [
    {"type": "message", "content": [{"type": "output_text", "text": "here is the answer"}]},
    {"type": "function_call", "call_id": "call_1", "name": "read", "arguments": "{\"path\":\"server.go\"}"}
  ],
  "usage": {"input_tokens": 100, "output_tokens": 20,
            "input_tokens_details": {"cached_tokens": 80, "cache_write_tokens": 15}}
}`

func TestExtractOpenAIResponsesUsage(t *testing.T) {
	refs := extractOpenAI(decode(t, responsesReply))

	if refs.usage == nil {
		t.Fatal("no usage read from a Responses reply — this is the silent zero-cost bug")
	}
	if refs.usage.InputTokens != 100 || refs.usage.OutputTokens != 20 {
		t.Errorf("tokens = %d in / %d out, want 100/20", refs.usage.InputTokens, refs.usage.OutputTokens)
	}
	if refs.usage.CacheReadTokens != 80 || refs.usage.CacheWriteTokens != 15 {
		t.Errorf("cache = %d read / %d write, want 80/15", refs.usage.CacheReadTokens, refs.usage.CacheWriteTokens)
	}
	if refs.model != "gpt-4.1" {
		t.Errorf("model = %q, want gpt-4.1", refs.model)
	}
}

// Content and tool calls must be visible to response hooks, and mutations must
// write back into the body that gets re-serialized — a reference that reads but
// does not write would let a plugin believe it had redacted something.
func TestExtractOpenAIResponsesContentIsMutable(t *testing.T) {
	body := decode(t, responsesReply)
	refs := extractOpenAI(body)

	if refs.content != "here is the answer" {
		t.Fatalf("content = %q, want %q", refs.content, "here is the answer")
	}
	if refs.setContent == nil {
		t.Fatal("content is not mutable — a plugin could not redact it")
	}
	refs.setContent("[redacted]")

	if len(refs.toolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(refs.toolCalls))
	}
	tc := refs.toolCalls[0]
	if tc.id != "call_1" || tc.name != "read" {
		t.Errorf("tool call = id %q name %q, want call_1/read", tc.id, tc.name)
	}
	if tc.argsJSON != `{"path":"server.go"}` {
		t.Errorf("arguments = %q", tc.argsJSON)
	}
	tc.setName("write")
	if err := tc.setArgs(`{"path":"other.go"}`); err != nil {
		t.Fatalf("setArgs: %v", err)
	}

	// Re-serialize: the mutations must be in the body, not just the refs.
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[redacted]", `"write"`, "other.go"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("mutation %s did not reach the response body: %s", want, out)
		}
	}
}

// The Chat path must keep working — it is the overwhelmingly common shape, and
// adding a branch for Responses is exactly the kind of change that quietly
// reroutes it.
func TestExtractOpenAIChatStillWorks(t *testing.T) {
	refs := extractOpenAI(decode(t, `{
      "model": "gpt-4",
      "choices": [{"message": {"content": "hello", "tool_calls": [
        {"id": "call_1", "function": {"name": "read", "arguments": "{\"path\":\"a.go\"}"}}]}}],
      "usage": {"prompt_tokens": 10, "completion_tokens": 5,
                "prompt_tokens_details": {"cached_tokens": 8}}
    }`))

	if refs.usage == nil || refs.usage.InputTokens != 10 || refs.usage.CacheReadTokens != 8 {
		t.Errorf("chat usage misread: %+v", refs.usage)
	}
	if refs.content != "hello" {
		t.Errorf("content = %q, want hello", refs.content)
	}
	if len(refs.toolCalls) != 1 || refs.toolCalls[0].name != "read" {
		t.Errorf("chat tool calls misread: %+v", refs.toolCalls)
	}
}
