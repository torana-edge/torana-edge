package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// Observed host-owned response facts must reach the plugin.
//
// `id` and `finish_reason` were hard-coded empty in the hook input, so a plugin
// received typed fields the provider had actually supplied and saw nothing.
// That is the same hostile silence v2 removes on the write side — a field that
// exists, validates, and is always empty.
func TestObservedResponseFactsAreExtractedPerFormat(t *testing.T) {
	for _, tc := range []struct {
		format     string
		body       string
		wantID     string
		wantFinish string
	}{
		{
			format:     "openai",
			body:       `{"id":"chatcmpl-1","model":"gpt-x","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
			wantID:     "chatcmpl-1",
			wantFinish: "stop",
		},
		{
			format:     "anthropic",
			body:       `{"id":"msg_1","model":"claude","stop_reason":"tool_use","content":[{"type":"text","text":"hi"}]}`,
			wantID:     "msg_1",
			wantFinish: "tool_use",
		},
		{
			format:     "bedrock",
			body:       `{"stopReason":"end_turn","output":{"message":{"content":[{"text":"hi"}]}}}`,
			wantFinish: "end_turn",
		},
		{
			format:     "gemini",
			body:       `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"hi"}]}}]}`,
			wantFinish: "STOP",
		},
	} {
		t.Run(tc.format, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
				t.Fatal(err)
			}
			refs := extractResponse(tc.format, body)
			if refs.id != tc.wantID {
				t.Errorf("id = %q, want %q", refs.id, tc.wantID)
			}
			if refs.finishReason != tc.wantFinish {
				t.Errorf("finish reason = %q, want %q", refs.finishReason, tc.wantFinish)
			}
		})
	}
}

// A provider signature must not survive a change to the content it covers.
//
// The mutation path did not carry signatures at all: toolCallRef had no
// signature field, so a plugin could rewrite a Gemini function call's arguments
// and the original thoughtSignature stayed in the outgoing JSON — a
// valid-looking provider token over content the provider never signed. B2 would
// have seen an empty accepted signature and been unable to detect or clear it.
func TestGeminiSignatureIsClearedWhenArgumentsChange(t *testing.T) {
	const body = `{"candidates":[{"finishReason":"STOP","content":{"parts":[
		{"thoughtSignature":"SIG_CALL_1","functionCall":{"id":"call_1","name":"search","args":{"q":"original"}}}
	]}}]}`

	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatal(err)
	}
	refs := extractResponse("gemini", decoded)
	if len(refs.toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(refs.toolCalls))
	}
	tc := refs.toolCalls[0]
	if tc.signature != "SIG_CALL_1" {
		t.Fatalf("signature was not extracted: %q — it must reach the pipeline, or a "+
			"stale token cannot be detected", tc.signature)
	}
	if tc.clearSignature == nil {
		t.Fatal("no way to clear the signature from the body")
	}

	// Simulate the accepted mutation: arguments change, so the token goes.
	if err := tc.setArgs(`{"q":"rewritten"}`); err != nil {
		t.Fatal(err)
	}
	tc.clearSignature()

	out, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "SIG_CALL_1") {
		t.Fatalf("the provider signature survived an argument change: %s", out)
	}
	if !strings.Contains(string(out), "rewritten") {
		t.Fatalf("the argument change was not applied: %s", out)
	}
}

// An untouched signed call keeps its token. Clearing unconditionally would
// discard provenance the provider legitimately supplied.
func TestGeminiSignatureSurvivesAnUntouchedCall(t *testing.T) {
	const body = `{"candidates":[{"content":{"parts":[
		{"thoughtSignature":"SIG_KEEP","functionCall":{"id":"c","name":"search","args":{"q":"x"}}}
	]}}]}`
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatal(err)
	}
	refs := extractResponse("gemini", decoded)
	if refs.toolCalls[0].signature != "SIG_KEEP" {
		t.Fatal("signature not extracted")
	}
	out, _ := json.Marshal(decoded)
	if !strings.Contains(string(out), "SIG_KEEP") {
		t.Fatalf("an untouched signature was dropped: %s", out)
	}
}
