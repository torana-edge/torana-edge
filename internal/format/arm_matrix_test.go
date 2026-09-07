package format_test

import (
	"testing"

	"github.com/torana-edge/torana-edge/internal/format/anthropic"
	"github.com/torana-edge/torana-edge/internal/format/gemini"
	"github.com/torana-edge/torana-edge/internal/format/openai"
)

// The provider-arm reference matrix (review finding 4): wire objects with
// conflicting content arms, or a declared arm missing its required members,
// are malformed known-format inputs — refused at the parse boundary with a
// value-free error, deterministically, never guessed through by switch
// precedence or map iteration. Every row is run repeatedly so a
// map-iteration order flip would fail the test.
const runs = 25

func expectParseError(t *testing.T, name, body string, unmarshal func([]byte) error) {
	t.Helper()
	for i := 0; i < runs; i++ {
		if err := unmarshal([]byte(body)); err == nil {
			t.Fatalf("run %d: ambiguous body accepted", i)
		}
	}
}

func TestGeminiArmMatrix(t *testing.T) {
	unmarshal := func(b []byte) error {
		_, err := (&gemini.Adapter{}).Unmarshal(b)
		return err
	}
	cases := map[string]string{
		"functionCall+functionResponse on one part": `{"model":"m","contents":[{"role":"model","parts":[{"functionCall":{"name":"r","args":{},"id":"c1"},"functionResponse":{"name":"r","response":{"out":"x"},"id":"c1"}}]}]}`,
		"text+functionCall on one part":             `{"model":"m","contents":[{"role":"model","parts":[{"text":"hi","functionCall":{"name":"r","args":{},"id":"c1"}}]}]}`,
		"thought+functionCall on one part":          `{"model":"m","contents":[{"role":"model","parts":[{"thought":true,"text":"r","functionCall":{"name":"r","args":{},"id":"c1"}}]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			expectParseError(t, name, body, unmarshal)
		})
	}
}

func TestOpenAIRequiredMemberMatrix(t *testing.T) {
	unmarshal := func(b []byte) error {
		_, err := (&openai.Adapter{}).Unmarshal(b)
		return err
	}
	cases := map[string]string{
		"text part without text member": `{"model":"m","messages":[{"role":"user","content":[{"type":"text"}]}]}`,
		"part without type member":      `{"model":"m","messages":[{"role":"user","content":[{"text":"hi"}]}]}`,
		"empty part":                    `{"model":"m","messages":[{"role":"user","content":[{}]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			expectParseError(t, name, body, unmarshal)
		})
	}
}

func TestAnthropicRequiredMemberMatrix(t *testing.T) {
	unmarshal := func(b []byte) error {
		_, err := (&anthropic.Adapter{}).Unmarshal(b)
		return err
	}
	cases := map[string]string{
		"text block without text member": `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text"}]}]}`,
		"tool_use with malformed input":  `{"model":"m","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"r","input":"nope"}]}]}`,

		"tool_use without id":                     `{"model":"m","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"r","input":{}}]}]}`,
		"tool_result without tool_use_id":         `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","content":"out"}]}]}`,
		"text block with tool-use members":        `{"model":"m","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"text","text":"hi","id":"c1","name":"r","input":{}}]}]}`,
		"text block with empty foreign id":        `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"hi","id":""}]}]}`,
		"text block with null foreign id":         `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"hi","id":null}]}]}`,
		"text block with null foreign input":      `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"hi","input":null}]}]}`,
		"text block with empty foreign object":    `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"hi","input":{}}]}]}`,
		"tool_use with empty foreign text":        `{"model":"m","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"r","input":{},"text":""}]}]}`,
		"tool_result with null foreign text":      `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"out","text":null}]}]}`,
		"text block with unknown additive member": `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"hi","future_field":1}]}]}`,
		"tool_use with unknown additive member":   `{"model":"m","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"r","input":{},"future_field":{}}]}]}`,
		"tool_use with text member":               `{"model":"m","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"r","input":{},"text":"x"}]}]}`,
		"tool_result with text member":            `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"out","text":"x"}]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			expectParseError(t, name, body, unmarshal)
		})
	}
}
