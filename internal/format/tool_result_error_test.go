package format_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
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
			if flag == "" {
				return
			}
			for _, variant := range []engine.OpenAIVariant{engine.OpenAIChat, engine.OpenAIResponses} {
				restored.OpenAIVariant = variant
				if _, err := (&openai.Adapter{}).Marshal(restored); err == nil {
					t.Fatal("OpenAI silently discarded is_error")
				}
			}
			if _, err := (&gemini.Adapter{}).Marshal(restored); err == nil {
				t.Fatal("Gemini silently discarded is_error")
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
