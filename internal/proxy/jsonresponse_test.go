package proxy

import (
	"context"
	"encoding/json"
	"os"
	"strings"
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
			t.Fatalf("%s missing — run 'make testdata' (err: %v)", path, err)
		}
		t.Skipf("%s not built — run 'make testdata'", path)
	}
}

func newPluginPipeline(t *testing.T, pluginDir string, order ...string) *plugin.PluginPipeline {
	t.Helper()
	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { rt.Close() })
	pp, err := plugin.NewPipeline(rt, plugin.PluginConfig{Dir: pluginDir, Order: order, AllowUnapproved: true})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if pp.Len() != len(order) {
		t.Fatalf("loaded %d plugins, want %d", pp.Len(), len(order))
	}
	return pp
}

// TestJSONResponseHooksAllFormats: tool-call arguments are routed through
// the plugin pipeline for every
// provider format, while sibling fields (id, usage, finish/stop reasons,
// unknown extras) survive untouched.
func TestJSONResponseHooksAllFormats(t *testing.T) {
	// This is host mechanics, not plugin behaviour, and gating it on a real
	// plugin took the ONLY cross-format test of runJSONResponseHooks out of
	// this repository's suite — including for the code path the Responses
	// accounting work rewrites.
	//
	// What it asserts is that the host can locate a tool call inside four
	// different provider body shapes, hand its arguments to a plugin, write the
	// result back, and leave every sibling field untouched. The plugin only has
	// to change a value; a fixture with a fixed substitution pins that far more
	// precisely than a real one whose output depends on its own logic.
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	bundles := fixturesDir

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
			format: "gemini",
			body: `{
				"modelVersion": "gemini-x",
				"candidates": [{"finishReason": "STOP", "content": {"role": "model", "parts": [
					{"thoughtSignature": "SIG_AFTER_RESPONSE", "functionCall": {"name": "write", "args": ` + kvArgsObj + `}}
				]}}],
				"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 5}
			}`,
			preserved: map[string]any{"modelVersion": "gemini-x"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			pp := newPluginPipeline(t, bundles, "test-mutator")

			out, err := runJSONResponseHooks(context.Background(), pp, 1, tc.format, nil, []byte(tc.body))
			if err != nil {
				t.Fatalf("runJSONResponseHooks: %v", err)
			}

			var body map[string]any
			if err := json.Unmarshal(out, &body); err != nil {
				t.Fatalf("output not valid JSON: %v", err)
			}

			// The host located the tool call, gave its arguments to the
			// plugin, and wrote the replacement back into this format's own
			// body shape. The fixture substitutes fixed bytes, so anything
			// else here is the host reading or writing the wrong place.
			refs := extractResponse(tc.format, body)
			if len(refs.toolCalls) != 1 {
				t.Fatalf("expected 1 tool call, got %d", len(refs.toolCalls))
			}
			var args map[string]any
			if err := json.Unmarshal([]byte(refs.toolCalls[0].argsJSON), &args); err != nil {
				t.Fatalf("args not valid JSON: %v (%q)", err, refs.toolCalls[0].argsJSON)
			}
			if args["mutated_by"] != "test-mutator" {
				t.Fatalf("the plugin's replacement did not reach this format's body: %v", args)
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

// The AFTER-RESPONSE mutation path must clear a provider signature too.
//
// The stream-path test covers a different call site of invalidateSignature.
// test-mutator rewrites tool arguments from run_after_response, so a signed
// Gemini body driven through it proves the second site — otherwise one branch
// was relying on the other's coverage.
func TestAfterResponseMutationClearsGeminiSignature(t *testing.T) {
	// fixturesDir, not officialBundlesDir: test-mutator is a repo fixture, and
	// gating this on TORANA_PLUGIN_BUNDLES_DIR would silently skip the
	// assertion in the default local run — which is exactly how a branch ends
	// up believing it is covered.
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newPluginPipeline(t, fixturesDir, "test-mutator")

	const body = `{
		"modelVersion": "gemini-x",
		"candidates": [{"finishReason": "STOP", "content": {"role": "model", "parts": [
			{"thoughtSignature": "SIG_AFTER_RESPONSE", "functionCall": {"id": "c1", "name": "write", "args": {"k": "v"}}}
		]}}]
	}`

	out, err := runJSONResponseHooks(context.Background(), pp, 1, "gemini", nil, []byte(body))
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}
	if !strings.Contains(string(out), "mutated_by") {
		t.Fatalf("test-mutator did not rewrite the arguments, so this proves nothing "+
			"about signature clearing: %s", out)
	}
	if strings.Contains(string(out), "SIG_AFTER_RESPONSE") {
		t.Fatalf("the provider signature survived an after-response mutation: %s", out)
	}
}
