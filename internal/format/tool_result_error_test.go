package format_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/torana-edge/torana-edge/internal/engine"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	"google.golang.org/protobuf/proto"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/format/anthropic"
	"github.com/torana-edge/torana-edge/internal/format/gemini"
	"github.com/torana-edge/torana-edge/internal/format/openai"
)

// Every boundary must preserve both presence and value: false is not absent,
// and a successful plugin round trip must never turn a failure into success.
func TestToolResultErrorAcrossAdapterAndGuestBridge(t *testing.T) {
	for _, flag := range []string{"", `,"is_error":false`, `,"is_error":true`} {
		t.Run(flag, func(t *testing.T) {
			a := &anthropic.Adapter{}
			body := fmt.Sprintf(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":"diagnostic"%s}]}]}`, flag)
			req, err := a.Unmarshal([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			wire, err := pbconv.ToPBChatRequestChecked(req)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := pbconv.FromPBChatRequest(wire)
			if err != nil {
				t.Fatal(err)
			}
			out, err := a.Marshal(restored)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Messages []struct{ Content []map[string]json.RawMessage }
			}
			if err := json.Unmarshal(out, &decoded); err != nil {
				t.Fatal(err)
			}
			got := string(decoded.Messages[0].Content[0]["is_error"])
			want := ""
			if flag != "" {
				want = flag[len(`,"is_error":`):]
			}
			if got != want {
				t.Fatalf("is_error = %q, want %q", got, want)
			}
		})
	}
}

func TestGeminiStructuredResultBecomesRecoverableError(t *testing.T) {
	a := &gemini.Adapter{}
	req, err := a.Unmarshal([]byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"read","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"secret"},"parts":[{"inlineData":{"mimeType":"text/plain","data":"c2VjcmV0"}}]}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := pbconv.ToPBChatRequestChecked(req)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := sdk.ReplaceToolResultWithError(wire.Messages[1], 0, "Sensitive output withheld."); err != nil || !changed {
		t.Fatalf("replace structured result: changed=%v err=%v", changed, err)
	}
	restored, err := pbconv.FromPBChatRequest(wire)
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Marshal(restored)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"error":"Sensitive output withheld."`)) || bytes.Contains(out, []byte("c2VjcmV0")) {
		t.Fatalf("Gemini error projection leaked or omitted content: %s", out)
	}
}

func TestAnthropicRecoverableErrorPreservesMultipleCacheBoundaries(t *testing.T) {
	a := &anthropic.Adapter{}
	req, err := a.Unmarshal([]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":[{"type":"text","text":"secret one","cache_control":{"type":"ephemeral","ttl":"5m"}},{"type":"text","text":"secret two","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := pbconv.ToPBChatRequestChecked(req)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := sdk.ReplaceToolResultWithError(wire.Messages[0], 0, "withheld"); err != nil || !changed {
		t.Fatalf("replace segmented result: changed=%v err=%v", changed, err)
	}
	restored, err := pbconv.FromPBChatRequest(wire)
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Marshal(restored)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("secret")) || bytes.Count(out, []byte(`"text":"withheld"`)) != 2 ||
		!bytes.Contains(out, []byte(`"ttl":"5m"`)) || !bytes.Contains(out, []byte(`"ttl":"1h"`)) {
		t.Fatalf("cache-delimited sanitized result was not representable: %s", out)
	}
}

func TestAnthropicToolResultErrorRequiresBoolean(t *testing.T) {
	for _, flag := range []string{"null", `"true"`, "1", "{}"} {
		body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":"diagnostic","is_error":%s}]}]}`, flag)
		if _, err := (&anthropic.Adapter{}).Unmarshal([]byte(body)); err == nil {
			t.Fatalf("accepted is_error=%s", flag)
		}
	}
}

// Formats without a boolean error flag still deliver the recoverable
// diagnostic in their native tool-result representation. Gemini has a native
// response.error convention; OpenAI carries the replacement text as output.
func TestCrossFormatToolErrorProjection(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		adapter    interface {
			Unmarshal([]byte) (*engine.ChatRequest, error)
			Marshal(*engine.ChatRequest) ([]byte, error)
		}
	}{
		{"openai-chat", `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"diagnostic"}]}`, &openai.Adapter{}},
		{"openai-responses", `{"model":"m","input":[{"type":"function_call","call_id":"c","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"diagnostic"}]}`, &openai.Adapter{}},
		{"gemini", `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"read","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"diagnostic"}}}]}]}`, &gemini.Adapter{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := tc.adapter.Unmarshal([]byte(tc.wire))
			if err != nil {
				t.Fatal(err)
			}
			var tr *engine.ToolResultBlock
			for _, m := range req.Messages {
				for _, b := range m.Blocks {
					if b.ToolResult != nil {
						tr = b.ToolResult
					}
				}
			}
			if tr == nil {
				t.Fatal("fixture contains no tool result")
			}
			for _, flag := range []*bool{nil, proto.Bool(false), proto.Bool(true)} {
				tr.IsError = flag
				out, err := tc.adapter.Marshal(req)
				if err != nil {
					t.Fatalf("tool result refused: %v", err)
				}
				if bytes.Contains(out, []byte(`"is_error"`)) {
					t.Fatal("canonical-only flag leaked onto wire")
				}
				if flag != nil && *flag && tc.name == "gemini" && !bytes.Contains(out, []byte(`"error"`)) {
					t.Fatalf("Gemini error convention missing: %s", out)
				}
			}
		})
	}
}
