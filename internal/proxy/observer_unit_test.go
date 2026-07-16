package proxy

import (
	"context"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

// TestObserverJSONHooksDirect drives runJSONResponseHooks with the
// test-observer fixture to isolate _response delivery from proxy transport.
func TestObserverJSONHooksDirect(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-observer/plugin.wasm")

	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { rt.Close() })
	pp, err := plugin.NewPipeline(rt, plugin.PluginConfig{Dir: "../../examples/plugins", Order: []string{"test-observer"}})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if pp.Len() != 1 {
		t.Fatalf("loaded %d plugins, want 1", pp.Len())
	}

	rs := &reqState{ID: 1, UpstreamStatus: 200, UsageIn: 7, UsageOut: 3}
	ctx := context.WithValue(context.Background(), reqStateKey{}, rs)
	chat := &engine.ChatRequest{Model: "gpt-x", ToranaMeta: map[string]any{}}
	body := []byte(`{"id":"x","model":"gpt-x","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)

	out, err := runJSONResponseHooks(ctx, pp, rs.ID, "openai", chat, body)
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}
	if !strings.Contains(string(out), "observed status=200 in=7 out=3") {
		t.Fatalf("observer mutation missing: %s", out)
	}
}
