package openai

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func TestChatContentPreservesWireSemantics(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		present   bool
		text      *string
		partCount int
		wantError bool
	}{
		{name: "absent", body: `{}`, present: false},
		{name: "null becomes empty scalar", body: `{"content":null}`, present: true, text: stringPtr("")},
		{name: "empty scalar", body: `{"content":""}`, present: true, text: stringPtr("")},
		{name: "scalar", body: `{"content":"hello"}`, present: true, text: stringPtr("hello")},
		{name: "parts", body: `{"content":[{"type":"text","text":"hello"}]}`, present: true, partCount: 1},
		{name: "wrong shape", body: `{"content":{"type":"text"}}`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				Content chatContent `json:"content"`
			}
			err := json.Unmarshal([]byte(tc.body), &got)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", err, tc.wantError)
			}
			if tc.wantError {
				return
			}
			if got.Content.Present != tc.present || len(got.Content.Parts) != tc.partCount {
				t.Fatalf("content=%+v", got.Content)
			}
			if tc.text == nil {
				if got.Content.Text != nil {
					t.Fatalf("text=%q want nil", *got.Content.Text)
				}
			} else if got.Content.Text == nil || *got.Content.Text != *tc.text {
				t.Fatalf("text=%v want %q", got.Content.Text, *tc.text)
			}
		})
	}
}

func TestLargeScalarContentDecodesExactly(t *testing.T) {
	content := bytes.Repeat([]byte("a"), 1<<20)
	body := append([]byte(`{"model":"gpt-4","messages":[{"role":"user","content":"`), content...)
	body = append(body, []byte(`"}]}`)...)

	got, err := (&Adapter{}).Unmarshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != engine.RoleUser ||
		got.Messages[0].Text() != string(content) {
		t.Fatal("large scalar content changed during decode")
	}
}

func BenchmarkUnmarshalLargeScalar(b *testing.B) {
	content := bytes.Repeat([]byte("a"), 1<<20)
	body := append([]byte(`{"model":"gpt-4","messages":[{"role":"user","content":"`), content...)
	body = append(body, []byte(`"}]}`)...)
	adapter := &Adapter{}
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for range b.N {
		if _, err := adapter.Unmarshal(body); err != nil {
			b.Fatal(err)
		}
	}
}

func stringPtr(s string) *string { return &s }
