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

func TestStripAdjacentNoticeElementsPreservesJSON(t *testing.T) {
	signer, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	notice, err := annotate.Render(signer, "conversation", suggest.Suggestion{ID: "sg_test", Code: "7f3k", Title: "Test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ordinary := range []bool{false, true} {
		parts := []any{}
		if ordinary {
			parts = append(parts, map[string]any{"type": "text", "text": "answer"})
		}
		parts = append(parts, map[string]any{"type": "text", "text": notice}, map[string]any{"type": "text", "text": notice})
		body, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": parts}}})
		got, changed, err := stripSignedNoticesJSON(body, "anthropic", signer, "conversation")
		if err != nil || !changed || !json.Valid(got) || bytes.Contains(got, []byte("torana:begin")) {
			t.Fatalf("strip=%s changed=%t err=%v", got, changed, err)
		}
		if ordinary && !bytes.Contains(got, []byte("answer")) {
			t.Fatal("ordinary text removed")
		}
	}
}

func TestStripNoticeOnlyAppendedElements(t *testing.T) {
	signer, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	notice, err := annotate.Render(signer, "conversation", suggest.Suggestion{ID: "sg_abc", Code: "7f3k", Title: "Try Opus"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		shape, original string
		makeBody        func(string) string
	}{
		{"anthropic", `{"messages":[{"role":"assistant","content":[{"type":"text","text":"answer"}]}]}`, func(n string) string {
			return `{"messages":[{"role":"assistant","content":[{"type":"text","text":"answer"},{"type":"text","text":` + n + `}]}]}`
		}},
		{"openai-responses", `{"input":[{"role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`, func(n string) string {
			return `{"input":[{"role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"role":"assistant","content":[{"type":"output_text","text":` + n + `}]}]}`
		}},
		{"gemini", `{"contents":[{"role":"model","parts":[{"text":"answer"}]}]}`, func(n string) string {
			return `{"contents":[{"role":"model","parts":[{"text":"answer"},{"text":` + n + `}]}]}`
		}},
		{"gemini-codeassist", `{"request":{"contents":[{"role":"model","parts":[{"text":"answer"}]}]}}`, func(n string) string {
			return `{"request":{"contents":[{"role":"model","parts":[{"text":"answer"},{"text":` + n + `}]}]}}`
		}},
	}
	noticeJSON, _ := json.Marshal(notice)
	for _, tc := range tests {
		t.Run(tc.shape, func(t *testing.T) {
			got, changed, err := stripSignedNoticesJSON([]byte(tc.makeBody(string(noticeJSON))), tc.shape, signer, "conversation")
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if string(got) != tc.original {
				t.Fatalf("cache prefix drift:\ngot  %s\nwant %s", got, tc.original)
			}
		})
	}
}

func TestStripWholeLocalReplyFromReplayedHistory(t *testing.T) {
	signer, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reply, err := annotate.RenderLocalReply(signer, "conversation", "local_1", "Suggestion accepted")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(reply)
	tests := []struct {
		shape, prefix, suffix, want string
	}{
		{"openai-chat", `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":`, `}]}`, `{"messages":[{"role":"user","content":"hello"}]}`},
		{"anthropic", `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":`, `}]}]}`, `{"messages":[{"role":"user","content":"hello"}]}`},
		{"openai-responses", `{"input":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"output_text","text":`, `}]}]}`, `{"input":[{"role":"user","content":"hello"}]}`},
		{"gemini", `{"contents":[{"role":"user","parts":[{"text":"hello"}]},{"role":"model","parts":[{"text":`, `}]}]}`, `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`},
		{"gemini-codeassist", `{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]},{"role":"model","parts":[{"text":`, `}]}]}}`, `{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`},
	}
	for _, tc := range tests {
		t.Run(tc.shape, func(t *testing.T) {
			body := []byte(tc.prefix + string(encoded) + tc.suffix)
			got, changed, err := stripSignedNoticesJSON(body, tc.shape, signer, "conversation")
			if err != nil || !changed || string(got) != tc.want {
				t.Fatalf("strip = %s, changed %v, err %v; want %s", got, changed, err, tc.want)
			}
		})
	}
}
