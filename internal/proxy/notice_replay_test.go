package proxy

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/torana-edge/torana-edge/internal/annotate"
	"github.com/torana-edge/torana-edge/internal/secret"
	"github.com/torana-edge/torana-edge/internal/suggest"
)

func TestStripSignedNoticesJSON(t *testing.T) {
	signer, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	notice, err := annotate.Render(signer, "conversation", suggest.Suggestion{ID: "sg_abc", Code: "7f3k", Title: "Try Opus"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		shape string
		body  map[string]any
	}{
		{"anthropic", map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "answer" + notice}}}}}},
		{"openai-chat", map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": "answer" + notice}}}},
		{"openai-responses", map[string]any{"input": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "answer" + notice}}}}}},
		{"gemini", map[string]any{"contents": []any{map[string]any{"role": "model", "parts": []any{map[string]any{"text": "answer" + notice}}}}}},
		{"gemini-codeassist", map[string]any{"request": map[string]any{"contents": []any{map[string]any{"role": "model", "parts": []any{map[string]any{"text": "answer" + notice}}}}}}},
	}
	for _, tc := range tests {
		t.Run(tc.shape, func(t *testing.T) {
			body, _ := json.Marshal(tc.body)
			body = bytes.TrimSuffix(body, []byte("}"))
			body = append(body, []byte(`,"extension":  9007199254740993}`)...)
			got, changed, err := stripSignedNoticesJSON(body, tc.shape, signer, "conversation")
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if bytes.Contains(got, []byte("torana:begin")) || !bytes.Contains(got, []byte("answer")) {
				t.Fatalf("notice not stripped: %s", got)
			}
			if !bytes.Contains(got, []byte(`"extension":  9007199254740993`)) {
				t.Fatal("unrelated wire bytes changed")
			}
			cross, changed, err := stripSignedNoticesJSON(body, tc.shape, signer, "other")
			if err != nil || changed || !bytes.Equal(cross, body) {
				t.Fatalf("cross-conversation changed=%v err=%v", changed, err)
			}
		})
	}
}
