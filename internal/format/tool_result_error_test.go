package format_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/torana-edge/torana-edge/internal/engine"
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

func TestAnthropicToolResultErrorRequiresBoolean(t *testing.T) {
	for _, flag := range []string{"null", `"true"`, "1", "{}"} {
		body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":"diagnostic","is_error":%s}]}]}`, flag)
		if _, err := (&anthropic.Adapter{}).Unmarshal([]byte(body)); err == nil {
			t.Fatalf("accepted is_error=%s", flag)
		}
	}
}

// Start with each provider's valid topology so a different marshal refusal
// cannot accidentally make the failure-flag assertion pass.
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
				if flag != nil && *flag {
					if err == nil {
						t.Fatal("true was silently dropped")
					}
					continue
				}
				if err != nil {
					t.Fatalf("successful tool result refused: %v", err)
				}
				if bytes.Contains(out, []byte(`"is_error"`)) {
					t.Fatal("unsupported flag leaked onto wire")
				}
			}
		})
	}
}
