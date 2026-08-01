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
	refs := extractOpenAI(decode(t, responsesReply), []byte(responsesReply))

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
	refs := extractOpenAI(body, []byte(responsesReply))

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
	const chatReply = `{
      "model": "gpt-4",
      "choices": [{"message": {"content": "hello", "tool_calls": [
        {"id": "call_1", "function": {"name": "read", "arguments": "{\"path\":\"a.go\"}"}}]}}],
      "usage": {"prompt_tokens": 10, "completion_tokens": 5,
                "prompt_tokens_details": {"cached_tokens": 8}}
    }`
	refs := extractOpenAI(decode(t, chatReply), []byte(chatReply))

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

// TestResponsesArgumentsStayAJSONString — the Responses wire format carries
// tool-call arguments as a JSON string. The first version reached objArgs when
// "arguments" was absent, whose setter writes back a decoded OBJECT: the Chat
// shape. A plugin rewriting such a call produced a body the client cannot
// parse.
func TestResponsesArgumentsStayAJSONString(t *testing.T) {
	for name, item := range map[string]string{
		"arguments absent":    `{"type":"function_call","call_id":"c1","name":"ping"}`,
		"arguments as string": `{"type":"function_call","call_id":"c1","name":"ping","arguments":"{\"a\":1}"}`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := `{"object":"response","model":"m","output":[` + item + `]}`
			body := decode(t, raw)
			refs := extractOpenAI(body, []byte(raw))

			if len(refs.toolCalls) != 1 {
				t.Fatalf("got %d tool calls", len(refs.toolCalls))
			}
			if err := refs.toolCalls[0].setArgs(`{"b":2}`); err != nil {
				t.Fatalf("setArgs: %v", err)
			}

			out, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			// A string on the wire, so the re-serialized form contains the
			// escaped JSON rather than a nested object.
			if !strings.Contains(string(out), `"arguments":"{\"b\":2}"`) {
				t.Errorf("arguments were not written back as a JSON string: %s", out)
			}
		})
	}
}

// Invalid JSON must be refused rather than written to the wire, where it would
// reach the client as a malformed tool call.
func TestResponsesArgumentsRejectInvalidJSON(t *testing.T) {
	const raw = `{"object":"response","model":"m","output":[{"type":"function_call","call_id":"c1","name":"ping","arguments":"{}"}]}`
	body := decode(t, raw)
	refs := extractOpenAI(body, []byte(raw))
	if err := refs.toolCalls[0].setArgs("not json"); err == nil {
		t.Error("invalid JSON was accepted into the tool call arguments")
	}
}

// TestResponsesRedactionReachesEveryTextPart — a Responses reply routinely
// carries several output_text parts. Binding only the first left the rest
// read-only, so a redaction plugin would report success having rewritten one
// paragraph of a multi-part answer, and the PII would still ship.
func TestResponsesRedactionReachesEveryTextPart(t *testing.T) {
	const raw = `{
      "object": "response", "model": "m",
      "output": [{"type":"message","content":[
        {"type":"output_text","text":"my email is "},
        {"type":"output_text","text":"alice@example.com"},
        {"type":"output_text","text":" — do not share"}
      ]}]
    }`
	body := decode(t, raw)
	refs := extractOpenAI(body, []byte(raw))

	if refs.content != "my email is alice@example.com — do not share" {
		t.Errorf("a plugin should see the whole reply, got %q", refs.content)
	}
	refs.setContent("[redacted]")

	out, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "alice@example.com") {
		t.Errorf("redaction left PII in a later text part: %s", out)
	}
	if !strings.Contains(string(out), "[redacted]") {
		t.Errorf("replacement text did not reach the body: %s", out)
	}
}
