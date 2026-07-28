package plugin

import (
	"context"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/wasm"
	"github.com/torana-edge/torana-plugin-sdk/pb"
)

func tickRequest(id uint64) *pb.TickRequest {
	return &pb.TickRequest{TickId: id, UnixMillis: 1769000000000, IntervalMs: 240000}
}

// findBundle locates one discovered bundle by manifest name.
func findBundle(t *testing.T, dir, name string) *PluginBundle {
	t.Helper()
	bundles, err := DiscoverPlugins(dir)
	if err != nil {
		t.Fatalf("DiscoverPlugins(%s): %v", dir, err)
	}
	for i := range bundles {
		if bundles[i].Manifest.Name == name {
			return &bundles[i]
		}
	}
	t.Fatalf("bundle %q not found in %s", name, dir)
	return nil
}

// runBeforeRequestForModel drives one request through the pipeline and returns
// the resulting model string. A tick leaves no request-scoped trace, so reading
// its effect back out through a subsequent request is the only way to observe
// that it ran.
func runBeforeRequestForModel(t *testing.T, pp *PluginPipeline, model string) string {
	t.Helper()
	out, err := pp.RunBeforeRequest(context.Background(), 99, &engine.ChatRequest{
		Model:    model,
		Messages: []engine.Message{{Role: engine.RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	return out.Model
}

// TestRunOnTick_EmptyPipeline — no plugins means no outcomes and no panic.
func TestRunOnTick_EmptyPipeline(t *testing.T) {
	rt := wasm.NewRuntime(context.Background())
	defer rt.Close()
	pp, err := NewPipeline(rt, PluginConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if got := pp.RunOnTick(context.Background(), 1, tickRequest(1)); len(got) != 0 {
		t.Errorf("empty pipeline produced outcomes: %+v", got)
	}
	if pp.TicksEnabled() {
		t.Error("TicksEnabled true with no plugins")
	}
}

// TestRunOnTick_SkippedWithoutGrant is the capability gate. Background execution
// is the one thing a plugin can do outside any request an operator can see, so
// running it without an explicit grant would be the worst kind of silent
// escalation.
func TestRunOnTick_SkippedWithoutGrant(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-ticker/plugin.wasm")

	rt := wasm.NewRuntime(context.Background())
	defer rt.Close()

	bundle := findBundle(t, "../../examples/plugins", "test-ticker")
	pl, err := rt.LoadPlugin(bundle.Manifest.Name, bundle.WASMBytes)
	if err != nil {
		t.Fatalf("LoadPlugin: %v", err)
	}
	pl.SetGrants(nil) // declares the hook, holds no grant

	pp := &PluginPipeline{
		plugins: []*loadedPlugin{{manifest: bundle.Manifest, plugin: pl}},
		runtime: rt,
		drained: make(chan struct{}),
		closed:  make(chan struct{}),
	}

	if got := pp.RunOnTick(context.Background(), 1, tickRequest(1)); len(got) != 0 {
		t.Errorf("a plugin without env.background_tick was ticked: %+v", got)
	}
	if pp.TicksEnabled() {
		t.Error("TicksEnabled true for a plugin holding no grant — the host would schedule a timer for nothing")
	}
}

// TestRunOnTick_FiresWithGrant is the end-to-end ABI check against a real
// guest: the host encodes a TickRequest, the guest decodes it, acts, and its
// TickResult comes back intact.
func TestRunOnTick_FiresWithGrant(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-ticker/plugin.wasm")

	pp := newTestPipeline(t, "../../examples/plugins", []string{"test-ticker"})

	if !pp.TicksEnabled() {
		t.Fatal("TicksEnabled false for a granted plugin declaring the hook")
	}

	// Tick 1 returns nil from the guest: "nothing to do".
	if got := pp.RunOnTick(context.Background(), 1, tickRequest(1)); len(got) != 0 {
		t.Errorf("a nil guest result was recorded as an action: %+v", got)
	}

	// Tick 2 returns a handled result.
	got := pp.RunOnTick(context.Background(), 2, tickRequest(2))
	if len(got) != 1 {
		t.Fatalf("got %d outcomes, want 1", len(got))
	}
	if got[0].Plugin != "test-ticker" {
		t.Errorf("outcome attributed to %q", got[0].Plugin)
	}
	if got[0].Actions != 2 {
		t.Errorf("Actions = %d, want 2 — the guest's count did not survive", got[0].Actions)
	}
	if got[0].Note != "tick 2" {
		t.Errorf("Note = %q, want %q", got[0].Note, "tick 2")
	}
}

// TestRunOnTick_DeliversHostClock — plugins have no clock of their own (WASI
// preview1 exposes none), so TickRequest is their only time source. If the host
// stopped populating it, a warming plugin would silently lose the ability to
// tell how long a cache had been idle.
func TestRunOnTick_DeliversHostClock(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-ticker/plugin.wasm")

	pp := newTestPipeline(t, "../../examples/plugins", []string{"test-ticker"})
	pp.RunOnTick(context.Background(), 1, tickRequest(7))

	// The guest wrote what it received into the shared cache; read it back
	// through a request, which is the only way to observe a tick's effects.
	chat := runBeforeRequestForModel(t, pp, "base")
	want := "base+id=7 millis=1769000000000 interval=240000"
	if chat != want {
		t.Errorf("guest saw %q, want %q", chat, want)
	}
}

// TestRunOnTick_PluginWithoutHookIgnored — a plugin that holds the grant but
// never declares the hook must not be called. CallRequest treats a missing
// export as silent success, so only the manifest check prevents a pointless
// call every tick.
func TestRunOnTick_PluginWithoutHookIgnored(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-metrics/plugin.wasm")

	pp := newTestPipeline(t, fixturesDir, []string{"test-metrics"})
	if got := pp.RunOnTick(context.Background(), 1, tickRequest(1)); len(got) != 0 {
		t.Errorf("a plugin that does not declare run_on_tick was ticked: %+v", got)
	}
	if pp.TicksEnabled() {
		t.Error("TicksEnabled true for a plugin that does not declare the hook")
	}
}
