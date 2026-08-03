package format_test

import (
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format/anthropic"
	"github.com/torana-edge/torana-edge/internal/format/bedrock"
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

func TestBedrockArmMatrix(t *testing.T) {
	unmarshal := func(b []byte) error {
		_, err := (&bedrock.Adapter{}).Unmarshal(b)
		return err
	}
	cases := map[string]string{
		"text+toolUse on one block":      `{"modelId":"m","messages":[{"role":"user","content":[{"text":"hi","toolUse":{"toolUseId":"c1","name":"r","input":{}}}]}]}`,
		"text+thinking on one block":     `{"modelId":"m","messages":[{"role":"assistant","content":[{"text":"hi","thinking":{"thinking":"r"}}]}]}`,
		"two unknown discriminants":      `{"modelId":"m","messages":[{"role":"user","content":[{"image":{"source":{"bytes":"x"}},"document":{"format":"pdf"}}]}]}`,
		"known arm plus unknown member":  `{"modelId":"m","messages":[{"role":"user","content":[{"text":"hi","custom":1}]}]}`,
		"nested two discriminants":       `{"modelId":"m","messages":[{"role":"user","content":[{"toolResult":{"toolUseId":"c1","content":[{"text":"a","json":{"x":1}}]}}]}]}`,
		"unknown role":                   `{"modelId":"m","messages":[{"role":"developer","content":[{"text":"hi"}]}]}`,
		"system role in messages":        `{"modelId":"m","messages":[{"role":"system","content":[{"text":"hi"}]}]}`,
		"system entry text+cachePoint":   `{"modelId":"m","system":[{"text":"hi","cachePoint":{"type":"default"}}],"messages":[{"role":"user","content":[{"text":"hi"}]}]}`,
		"system entry empty":             `{"modelId":"m","system":[{}],"messages":[{"role":"user","content":[{"text":"hi"}]}]}`,
		"toolConfig toolSpec+cachePoint": `{"modelId":"m","toolConfig":{"tools":[{"toolSpec":{"name":"r","inputSchema":{"json":{"type":"object"}}},"cachePoint":{"type":"default"}}]},"messages":[{"role":"user","content":[{"text":"hi"}]}]}`,
		"toolConfig entry empty":         `{"modelId":"m","toolConfig":{"tools":[{}]},"messages":[{"role":"user","content":[{"text":"hi"}]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			expectParseError(t, name, body, unmarshal)
		})
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

// TestBedrockRolesPreserved — the normative two-role grammar is preserved
// exactly: user and assistant survive to the engine verbatim (grant
// classification falls to ir.messages.write.user/assistant).
func TestBedrockRolesPreserved(t *testing.T) {
	chat, err := (&bedrock.Adapter{}).Unmarshal([]byte(`{"modelId":"m","messages":[{"role":"user","content":[{"text":"u"}]},{"role":"assistant","content":[{"text":"a"}]}]}`))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(chat.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(chat.Messages))
	}
	if string(chat.Messages[0].Role) != "user" || string(chat.Messages[1].Role) != "assistant" {
		t.Fatalf("roles not preserved: %+v", chat.Messages)
	}
	// The unmodelled-role case must be a parse error, never a silent relabel.
	_, err = (&bedrock.Adapter{}).Unmarshal([]byte(`{"modelId":"m","messages":[{"role":"other","content":[{"text":"x"}]}]}`))
	if err == nil {
		t.Fatal("unmodelled bedrock role must be rejected")
	}
	if !strings.Contains(err.Error(), "role") {
		t.Fatalf("error = %q, want the role condition named", err)
	}

	// The marshal side refuses unmodelled engine roles identically.
	marshalChat := &engine.ChatRequest{
		Model:    "m",
		Messages: []engine.Message{{Role: "other", Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "x"}}}}},
	}
	if _, err := (&bedrock.Adapter{}).Marshal(marshalChat); err == nil {
		t.Fatal("unmodelled engine role must be rejected on marshal")
	}
}
