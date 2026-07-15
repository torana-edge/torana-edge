package proxy

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

// requireWASM skips locally when the plugin binary is missing but fails in
// CI (TORANA_E2E=1) so missing binaries can never silently disable coverage.
func requireWASM(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		if os.Getenv("TORANA_E2E") != "" {
			t.Fatalf("%s missing — run 'make plugins testdata' (err: %v)", path, err)
		}
		t.Skipf("%s not built — run 'make plugins testdata'", path)
	}
}

func newPluginPipeline(t *testing.T, order ...string) *plugin.PluginPipeline {
	t.Helper()
	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { rt.Close() })
	pp, err := plugin.NewPipeline(rt, plugin.PluginConfig{Dir: "../../plugins", Order: order})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if pp.Len() != len(order) {
		t.Fatalf("loaded %d plugins, want %d", pp.Len(), len(order))
	}
	return pp
}

// TestJSONResponseHooksAllFormats: tool-call arguments are routed through
// the plugin pipeline (KV arrays reversed by schema_translator) for every
// provider format, while sibling fields (id, usage, finish/stop reasons,
// unknown extras) survive untouched.
func TestJSONResponseHooksAllFormats(t *testing.T) {
	requireWASM(t, "../../plugins/schema_translator/plugin.wasm")

	kvArgsStr := `{"env":[{"key":"A","value":"1"}]}` // openai: JSON string
	kvArgsObj := `{"env":[{"key":"A","value":"1"}]}` // object formats: raw object

	cases := []struct {
		format string
		body   string
		// dotted paths (resolved by walk) that must survive unchanged
		preserved map[string]any
		// path to the reversed args object
		argsPath []string
	}{
		{
			format: "openai",
			body: `{
				"id": "chatcmpl-42", "object": "chat.completion", "model": "gpt-x",
				"system_fingerprint": "fp_abc",
				"choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
					"role": "assistant", "content": null,
					"tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "write", "arguments": ` + jsonStr(kvArgsStr) + `}}]
				}}],
				"usage": {"prompt_tokens": 10, "completion_tokens": 5}
			}`,
			preserved: map[string]any{
				"id":                 "chatcmpl-42",
				"system_fingerprint": "fp_abc",
			},
		},
		{
			format: "anthropic",
			body: `{
				"id": "msg_42", "type": "message", "model": "claude-x", "stop_reason": "tool_use",
				"content": [
					{"type": "text", "text": "let me check"},
					{"type": "tool_use", "id": "toolu_1", "name": "write", "input": ` + kvArgsObj + `}
				],
				"usage": {"input_tokens": 10, "output_tokens": 5}
			}`,
			preserved: map[string]any{
				"id":          "msg_42",
				"stop_reason": "tool_use",
			},
		},
		{
			format: "bedrock",
			body: `{
				"stopReason": "tool_use",
				"output": {"message": {"role": "assistant", "content": [
					{"toolUse": {"toolUseId": "tooluse_1", "name": "write", "input": ` + kvArgsObj + `}}
				]}},
				"usage": {"inputTokens": 10, "outputTokens": 5}
			}`,
			preserved: map[string]any{"stopReason": "tool_use"},
		},
		{
			format: "vertex",
			body: `{
				"modelVersion": "gemini-x",
				"candidates": [{"finishReason": "STOP", "content": {"role": "model", "parts": [
					{"functionCall": {"name": "write", "args": ` + kvArgsObj + `}}
				]}}],
				"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 5}
			}`,
			preserved: map[string]any{"modelVersion": "gemini-x"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			pp := newPluginPipeline(t, "schema_translator")

			out, err := runJSONResponseHooks(context.Background(), pp, 1, tc.format, nil, []byte(tc.body))
			if err != nil {
				t.Fatalf("runJSONResponseHooks: %v", err)
			}

			var body map[string]any
			if err := json.Unmarshal(out, &body); err != nil {
				t.Fatalf("output not valid JSON: %v", err)
			}

			// Args reversed by the plugin.
			refs := extractResponse(tc.format, body)
			if len(refs.toolCalls) != 1 {
				t.Fatalf("expected 1 tool call, got %d", len(refs.toolCalls))
			}
			var args map[string]any
			if err := json.Unmarshal([]byte(refs.toolCalls[0].argsJSON), &args); err != nil {
				t.Fatalf("args not valid JSON: %v (%q)", err, refs.toolCalls[0].argsJSON)
			}
			env, ok := args["env"].(map[string]any)
			if !ok || env["A"] != "1" {
				t.Fatalf("expected env reversed to object, got %v", args)
			}

			// Sibling fields preserved.
			for key, want := range tc.preserved {
				if got := body[key]; got != want {
					t.Errorf("%s: got %v want %v", key, got, want)
				}
			}
			if body["usage"] == nil && body["usageMetadata"] == nil {
				t.Error("usage dropped from response")
			}
		})
	}
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
