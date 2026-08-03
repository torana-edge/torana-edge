package format_test

import (
	"math"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/format/anthropic"
	"github.com/torana-edge/torana-edge/internal/format/bedrock"
	"github.com/torana-edge/torana-edge/internal/format/gemini"
	"github.com/torana-edge/torana-edge/internal/format/openai"
)

// The accepted-domain closure: max_tokens (and provider equivalents) must
// be within 1..MaxInt32 — anything else is refused at parse (the 400-class
// gate), never clamped, never re-emitted. One over/under/valid case per
// adapter, with the exact provider member name.
func TestMaxTokensDomainClosure(t *testing.T) {
	cases := []struct {
		name    string
		adapter interface {
			Unmarshal([]byte) (any, error)
		}
		valid   string
		over    string
		under   string
		overVal string
	}{
		{
			name: "anthropic",
			adapter: adaptFn(func(b []byte) (any, error) {
				return (&anthropic.Adapter{}).Unmarshal(b)
			}),
			valid: `{"model":"m","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`,
			over:  `{"model":"m","max_tokens":2147483648,"messages":[{"role":"user","content":"hi"}]}`,
			under: `{"model":"m","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "openai chat",
			adapter: adaptFn(func(b []byte) (any, error) {
				return (&openai.Adapter{}).Unmarshal(b)
			}),
			valid: `{"model":"m","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`,
			over:  `{"model":"m","max_tokens":2147483648,"messages":[{"role":"user","content":"hi"}]}`,
			under: `{"model":"m","max_tokens":-1,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "gemini",
			adapter: adaptFn(func(b []byte) (any, error) {
				return (&gemini.Adapter{}).Unmarshal(b)
			}),
			valid: `{"model":"m","generationConfig":{"maxOutputTokens":4096},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			over:  `{"model":"m","generationConfig":{"maxOutputTokens":2147483648},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			under: `{"model":"m","generationConfig":{"maxOutputTokens":0},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
		},
		{
			name: "bedrock",
			adapter: adaptFn(func(b []byte) (any, error) {
				return (&bedrock.Adapter{}).Unmarshal(b)
			}),
			valid: `{"modelId":"m","inferenceConfig":{"maxTokens":4096},"messages":[{"role":"user","content":[{"text":"hi"}]}]}`,
			over:  `{"modelId":"m","inferenceConfig":{"maxTokens":2147483648},"messages":[{"role":"user","content":[{"text":"hi"}]}]}`,
			under: `{"modelId":"m","inferenceConfig":{"maxTokens":-5},"messages":[{"role":"user","content":[{"text":"hi"}]}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.adapter.Unmarshal([]byte(tc.valid)); err != nil {
				t.Fatalf("valid max_tokens refused: %v", err)
			}
			for _, bad := range []struct{ name, body string }{
				{"over", tc.over}, {"under", tc.under},
			} {
				_, err := tc.adapter.Unmarshal([]byte(bad.body))
				if err == nil {
					t.Fatalf("%s max_tokens accepted", bad.name)
				}
				if !strings.Contains(err.Error(), "outside 1..") {
					t.Errorf("%s error = %q, want the domain condition named", bad.name, err)
				}
			}
			_ = math.MaxInt32 // keep the boundary import honest
		})
	}
}

// adaptFn adapts a provider Unmarshal to the uniform signature.
type adaptFn func([]byte) (any, error)

func (f adaptFn) Unmarshal(b []byte) (any, error) { return f(b) }
