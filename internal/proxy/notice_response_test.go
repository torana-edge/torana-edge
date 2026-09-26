package proxy

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestAppendNoticeJSON(t *testing.T) {
	tests := []struct {
		name, shape, body string
		want              bool
	}{
		{"anthropic", "anthropic", `{"stop_reason":"end_turn","content":[{"type":"text","text":"hello"}],"usage":{"input_tokens":  3}}`, true},
		{"anthropic tool", "anthropic", `{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"x"}]}`, false},
		{"chat", "openai-chat", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hello"}}],"usage":{"total_tokens":  3}}`, true},
		{"chat tool", "openai-chat", `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{}]}}]}`, false},
		{"responses", "openai-responses", `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"total_tokens":  3}}`, true},
		{"responses tool", "openai-responses", `{"status":"completed","output":[{"type":"function_call","name":"read"}]}`, false},
		{"gemini", "gemini", `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"hello"}]}}],"usageMetadata":{"totalTokenCount":  3}}`, true},
		{"gemini tool", "gemini", `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"read"}}]}}]}`, false},
		{"codeassist", "gemini-codeassist", `{"response":{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"hello"}]}}]},"trace":  3}`, true},
		{"codeassist tool", "gemini-codeassist", `{"response":{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"read"}}]}}]}}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, changed, err := appendNoticeJSON([]byte(tc.body), tc.shape, "\nTorana notice", "abc")
			if err != nil {
				t.Fatal(err)
			}
			if changed != tc.want {
				t.Fatalf("changed=%v, want %v", changed, tc.want)
			}
			if !tc.want {
				if string(got) != tc.body {
					t.Fatal("skipped response was changed")
				}
				return
			}
			if !bytes.Contains(got, []byte("Torana notice")) {
				t.Fatal("notice missing")
			}
			var decoded any
			if err := json.Unmarshal(got, &decoded); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains([]byte(tc.body), []byte(":  3")) && !bytes.Contains(got, []byte(":  3")) {
				t.Fatal("unrelated JSON bytes changed")
			}
		})
	}
}
