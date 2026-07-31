package proxy

import (
	"encoding/json"
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
