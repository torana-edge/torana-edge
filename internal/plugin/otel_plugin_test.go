package plugin

import (
	"context"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// TestMetricFixtureEmitMetricABI exercises the labelled emit_metric host ABI on
// BOTH hooks: if the wasmimport signature and the host export disagree, the
// module traps at instantiation or on the call.
//
// The fixture must declare both hooks for this to mean anything. It originally
// declared only run_before_request, so RunAfterResponse dispatched to nothing
// (RunAfterResponse skips plugins whose manifest omits the hook) and the second
// half of the test passed for the wrong reason — a regression against the otel
// plugin it replaced, whose manifest declared both.
func TestMetricFixtureEmitMetricABI(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-metrics/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-metrics"})
	// Without this, dropping a hook from the manifest turns half the test into
	// a no-op that still passes.
	requireFixtureDeclaresHooks(t, "test-metrics", "run_before_request", "run_after_response")

	chat := &engine.ChatRequest{
		Model: "gpt-x",
		Messages: []engine.Message{
			{Role: engine.RoleUser, Content: "hello"},
		},
		Tools: []engine.ToolDef{{Name: "read"}},
	}
	if _, err := pp.RunBeforeRequest(context.Background(), 1, chat); err != nil {
		t.Fatalf("run_before_request: %v", err)
	}
	// v2 hands run_after_response a real ChatResponse. Passing the request
	// here was the v1 defect: a plugin reading the assistant's reply got the
	// conversation history instead.
	resp := &engine.ChatResponse{
		Model:          chat.Model,
		Message:        &engine.Message{Role: engine.RoleAssistant, Content: "hi"},
		UpstreamStatus: 200,
	}
	if _, err := pp.RunAfterResponse(context.Background(), 1, resp, true); err != nil {
		t.Fatalf("run_after_response: %v", err)
	}
}

// requireFixtureDeclaresHooks fails when a fixture's manifest omits a hook the
// test intends to exercise. RunAfterResponse and friends skip plugins that do
// not declare the hook, so a missing declaration turns an assertion into a
// silent no-op rather than a failure.
func requireFixtureDeclaresHooks(t *testing.T, name string, hooks ...string) {
	t.Helper()
	bundles, err := DiscoverPlugins(fixturesDir)
	if err != nil {
		t.Fatalf("discover fixtures: %v", err)
	}
	for _, b := range bundles {
		if b.Manifest.Name != name {
			continue
		}
		declared := make(map[string]bool, len(b.Manifest.Hooks))
		for _, h := range b.Manifest.Hooks {
			declared[h.Name] = true
		}
		for _, want := range hooks {
			if !declared[want] {
				t.Fatalf("fixture %q does not declare %s, so a test calling it dispatches "+
					"to nothing and passes for the wrong reason", name, want)
			}
		}
		return
	}
	t.Fatalf("fixture %q not found in %s", name, fixturesDir)
}
