package proxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

// Observed host-owned response facts must reach the plugin.
//
// `id` and `finish_reason` were hard-coded empty in the hook input, so a plugin
// received typed fields the provider had actually supplied and saw nothing.
// That is the same hostile silence v2 removes on the write side — a field that
// exists, validates, and is always empty.
func TestObservedResponseFactsAreExtractedPerFormat(t *testing.T) {
	for _, tc := range []struct {
		format     string
		body       string
		wantID     string
		wantFinish string
	}{
		{
			format:     "openai",
			body:       `{"id":"chatcmpl-1","model":"gpt-x","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
			wantID:     "chatcmpl-1",
			wantFinish: "stop",
		},
		{
			format:     "anthropic",
			body:       `{"id":"msg_1","model":"claude","stop_reason":"tool_use","content":[{"type":"text","text":"hi"}]}`,
			wantID:     "msg_1",
			wantFinish: "tool_use",
		},
		{
			format:     "bedrock",
			body:       `{"stopReason":"end_turn","output":{"message":{"content":[{"text":"hi"}]}}}`,
			wantFinish: "end_turn",
		},
		{
			format:     "gemini",
			body:       `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"hi"}]}}]}`,
			wantFinish: "STOP",
		},
	} {
		t.Run(tc.format, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
				t.Fatal(err)
			}
			refs := extractResponse(tc.format, body)
			if refs.id != tc.wantID {
				t.Errorf("id = %q, want %q", refs.id, tc.wantID)
			}
			if refs.finishReason != tc.wantFinish {
				t.Errorf("finish reason = %q, want %q", refs.finishReason, tc.wantFinish)
			}
		})
	}
}

// A provider signature must not survive a change to the content it covers —
// asserted THROUGH runJSONResponseHooks, with a real mutating plugin.
//
// The previous version of this test called setArgs and clearSignature itself,
// so it would have passed even if the host never cleared anything. It tested
// the helpers, not the behaviour.
func TestPipelineClearsSignatureWhenAStreamHookRewritesArguments(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-tool-rewriter/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-tool-rewriter"})

	const body = `{"candidates":[{"finishReason":"STOP","content":{"parts":[
		{"thoughtSignature":"SIG_CALL_1","functionCall":{"id":"call_1","name":"search","args":{"q":"original"}}}
	]}}]}`

	out, err := runJSONResponseHooks(context.Background(), pp, 1, "gemini",
		&engine.ChatRequest{Model: "gemini-x"}, []byte(body))
	if err != nil {
		t.Fatalf("hooks: %v", err)
	}
	if !strings.Contains(string(out), "rewritten-by-plugin") {
		t.Fatalf("the plugin did not rewrite the arguments, so this test proves "+
			"nothing about signature clearing: %s", out)
	}
	if strings.Contains(string(out), "SIG_CALL_1") {
		t.Fatalf("the provider signature survived a STREAM-hook rewrite: %s", out)
	}
}

// An untouched signed call keeps its token. Clearing unconditionally would
// discard provenance the provider legitimately supplied.
func TestPipelinePreservesSignatureWhenNothingChanges(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-inert-a/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-inert-a"})

	const body = `{"candidates":[{"content":{"parts":[
		{"thoughtSignature":"SIG_KEEP","functionCall":{"id":"c","name":"search","args":{"q":"x"}}}
	]}}]}`

	out, err := runJSONResponseHooks(context.Background(), pp, 2, "gemini",
		&engine.ChatRequest{Model: "gemini-x"}, []byte(body))
	if err != nil {
		t.Fatalf("hooks: %v", err)
	}
	if !strings.Contains(string(out), "SIG_KEEP") {
		t.Fatalf("an untouched signature was dropped: %s", out)
	}
}

// newProxyTestPipeline builds a pipeline over the fixture bundles, so response
// tests can drive runJSONResponseHooks with real guests rather than calling its
// helpers directly.
func newProxyTestPipeline(t *testing.T, order []string) *plugin.PluginPipeline {
	t.Helper()
	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { _ = rt.Close() })
	pp, err := plugin.NewPipeline(rt, plugin.PluginConfig{
		Dir: fixturesDir, Order: order, AllowUnapproved: true,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return pp
}
