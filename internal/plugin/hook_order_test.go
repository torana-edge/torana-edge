package plugin

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

func hookOrderBundle(name string, hooks ...string) PluginBundle {
	manifestHooks := make([]Hook, len(hooks))
	for i, hook := range hooks {
		manifestHooks[i] = Hook{Name: hook}
	}
	return PluginBundle{Manifest: PluginManifest{
		ID:          "test/" + name,
		Name:        name,
		FailureMode: "pass",
		Hooks:       manifestHooks,
	}}
}

func hookOrderBundles(bundles ...PluginBundle) map[string]PluginBundle {
	out := make(map[string]PluginBundle, len(bundles))
	for _, bundle := range bundles {
		out[bundle.Manifest.Name] = bundle
	}
	return out
}

func TestHookOrderValidationExactAndFailClosed(t *testing.T) {
	bundles := hookOrderBundles(
		hookOrderBundle("a", "run_before_request", "run_after_response"),
		hookOrderBundle("b", "run_before_request"),
		hookOrderBundle("c", "run_before_request", "run_on_stream_chunk"),
	)
	base := PluginConfig{Order: []string{"a", "b", "c"}}

	valid := base
	valid.HookOrder = map[string][]string{
		"run_before_request":  {"c", "a", "b"},
		"run_after_response":  {"a"},
		"run_on_stream_chunk": {"c"},
		"run_on_tick":         {},
	}
	if err := validateHookOrders(valid, bundles); err != nil {
		t.Fatalf("valid hook order: %v", err)
	}
	if err := validateHookOrders(base, bundles); err != nil {
		t.Fatalf("omitted overrides must retain global order: %v", err)
	}

	tests := []struct {
		name      string
		hookOrder map[string][]string
		want      string
	}{
		{"unknown hook", map[string][]string{"future_hook": {}}, "unsupported hook"},
		{"targeted HTTP", map[string][]string{"run_on_http_request": {}}, "unsupported hook"},
		{"duplicate", map[string][]string{"run_before_request": {"a", "b", "b"}}, "duplicate"},
		{"missing", map[string][]string{"run_before_request": {"a", "b"}}, "missing plugin \"c\""},
		{"not enabled", map[string][]string{"run_before_request": {"a", "b", "c", "d"}}, "not in plugins.order"},
		{"wrong hook", map[string][]string{"run_after_response": {"b"}}, "does not declare"},
		{"missing bundle", map[string][]string{"run_before_request": {"a", "b", "c"}}, "missing or malformed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.HookOrder = tt.hookOrder
			gotBundles := bundles
			if tt.name == "missing bundle" {
				gotBundles = hookOrderBundles(bundles["a"], bundles["b"])
			}
			err := validateHookOrders(cfg, gotBundles)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestHookOrderPreservesSharedDependencies(t *testing.T) {
	upstream := hookOrderBundle("upstream", "run_before_request", "run_after_response")
	dependent := hookOrderBundle("dependent", "run_before_request", "run_after_response")
	dependent.Manifest.RequiresUpstream = []string{upstream.Manifest.ID}
	bundles := hookOrderBundles(upstream, dependent)

	cfg := PluginConfig{
		Order: []string{"upstream", "dependent"},
		HookOrder: map[string][]string{
			"run_before_request": {"dependent", "upstream"},
		},
	}
	if err := validateHookOrders(cfg, bundles); err == nil || !strings.Contains(err.Error(), "requires plugin id") {
		t.Fatalf("shared dependency inversion error = %v", err)
	}

	// If the upstream does not implement a hook, it is not part of that
	// hook's exact set and no synthetic cross-hook dependency is invented.
	upstream.Manifest.Hooks = []Hook{{Name: "run_before_request"}}
	bundles = hookOrderBundles(upstream, dependent)
	cfg.HookOrder = map[string][]string{"run_after_response": {"dependent"}}
	if err := validateHookOrders(cfg, bundles); err != nil {
		t.Fatalf("non-shared hook dependency rejected: %v", err)
	}
}

func TestHookOrderRouteMustPrecedeCompaction(t *testing.T) {
	router := hookOrderBundle("router", "run_before_request")
	router.Manifest.Permissions = []Permission{{Name: "env.route_request"}}
	router.Digest = "router-digest"
	gate := hookOrderBundle("gate", "run_before_request")
	gate.Manifest.Permissions = []Permission{{Name: "env.host_call.torana_evaluate_compaction"}}
	gate.Digest = "gate-digest"
	bundles := hookOrderBundles(router, gate)

	cfg := PluginConfig{
		Order:           []string{"router", "gate"},
		HookOrder:       map[string][]string{"run_before_request": {"gate", "router"}},
		AllowUnapproved: true,
	}
	if err := validateBeforeHookEconomicOrder(cfg, bundles); err == nil || !strings.Contains(err.Error(), "route-capable") {
		t.Fatalf("economic inversion error = %v", err)
	}
	cfg.HookOrder["run_before_request"] = []string{"router", "gate"}
	if err := validateBeforeHookEconomicOrder(cfg, bundles); err != nil {
		t.Fatalf("valid economic order: %v", err)
	}
}

func TestLoadedHookOrdersAreImmutableAndPerHook(t *testing.T) {
	a := &loadedPlugin{manifest: hookOrderBundle("a", "run_before_request", "run_after_response").Manifest}
	b := &loadedPlugin{manifest: hookOrderBundle("b", "run_before_request").Manifest}
	c := &loadedPlugin{manifest: hookOrderBundle("c", "run_before_request", "run_after_response").Manifest}
	loaded := []*loadedPlugin{a, b, c}
	overrides := map[string][]string{
		"run_before_request": {"c", "a", "b"},
		"run_after_response": {"a", "c"},
	}

	names := func(plugins []*loadedPlugin) []string {
		out := make([]string, len(plugins))
		for i, lp := range plugins {
			out[i] = lp.manifest.Name
		}
		return out
	}
	before := loadedPluginsForHook(loaded, overrides, "run_before_request")
	after := loadedPluginsForHook(loaded, overrides, "run_after_response")
	if !slices.Equal(names(before), []string{"c", "a", "b"}) || !slices.Equal(names(after), []string{"a", "c"}) {
		t.Fatalf("orders: before=%v after=%v", names(before), names(after))
	}
	// Mutating operator input after construction cannot alter a generation.
	overrides["run_before_request"][0] = "a"
	loaded[0] = c
	if !slices.Equal(names(before), []string{"c", "a", "b"}) {
		t.Fatalf("constructed order aliased mutable config: %v", names(before))
	}
	if got := names(loadedPluginsForHook([]*loadedPlugin{a, b, c}, nil, "run_after_response")); !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("default hook order = %v", got)
	}
	if got := names(loadedPluginsForHook([]*loadedPlugin{a, c}, map[string][]string{
		"run_before_request": {"c", "b", "a"},
	}, "run_before_request")); !slices.Equal(got, []string{"c", "a"}) {
		t.Fatalf("skipped plugin changed survivor order: %v", got)
	}

	pp := &PluginPipeline{streamPlugins: []*loadedPlugin{c, a}}
	vs := newStreamVerifierState(pp)
	if len(vs.plugins) != 2 || vs.plugins[0].lp != c || vs.plugins[1].lp != a {
		t.Fatalf("stream verifier is not aligned with stream order: %+v", vs.plugins)
	}
}

func TestBeforeRequestDispatchUsesHookOrder(t *testing.T) {
	const fixtures = "../../examples/plugins"
	for _, name := range []string{"test-inert-a", "test-inert-b", "test-inert-c"} {
		requireWASM(t, fixtures+"/"+name+"/plugin.wasm")
	}
	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { _ = rt.Close() })
	pp, err := NewPipeline(rt, PluginConfig{
		Dir:             fixtures,
		Order:           []string{"test-inert-a", "test-inert-b", "test-inert-c"},
		HookOrder:       map[string][]string{"run_before_request": {"test-inert-c", "test-inert-a", "test-inert-b"}},
		AllowUnapproved: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	const reqID = 991
	if _, err := pp.RunBeforeRequest(context.Background(), reqID, &engine.ChatRequest{Model: "m"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := pp.InvokedPlugins(reqID); !slices.Equal(got, []string{"test-inert-c", "test-inert-a", "test-inert-b"}) {
		t.Fatalf("dispatch order = %v", got)
	}
}

func TestAfterResponseDispatchUsesHookOrder(t *testing.T) {
	const fixtures = "../../examples/plugins"
	order := []string{"test-observer", "test-metrics", "test-mutator"}
	for _, name := range order {
		requireWASM(t, fixtures+"/"+name+"/plugin.wasm")
	}
	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { _ = rt.Close() })
	pp, err := NewPipeline(rt, PluginConfig{
		Dir:             fixtures,
		Order:           order,
		HookOrder:       map[string][]string{"run_after_response": {"test-mutator", "test-observer", "test-metrics"}},
		AllowUnapproved: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	const reqID = 992
	if _, err := pp.RunAfterResponse(context.Background(), reqID, &engine.ChatResponse{UpstreamStatus: 200}, true); err != nil {
		t.Fatal(err)
	}
	if got := pp.InvokedPlugins(reqID); !slices.Equal(got, []string{"test-mutator", "test-observer", "test-metrics"}) {
		t.Fatalf("dispatch order = %v", got)
	}
}

func TestStreamDispatchUsesHookOrder(t *testing.T) {
	const fixtures = "../../examples/plugins"
	order := []string{"test-stream-mutator", "test-stream-fanout", "test-stream-delay-stop"}
	for _, name := range order {
		requireWASM(t, fixtures+"/"+name+"/plugin.wasm")
	}
	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { _ = rt.Close() })
	pp, err := NewPipeline(rt, PluginConfig{
		Dir:             fixtures,
		Order:           order,
		HookOrder:       map[string][]string{"run_on_stream_chunk": {"test-stream-fanout", "test-stream-delay-stop", "test-stream-mutator"}},
		AllowUnapproved: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	const reqID = 993
	text := "hello"
	if _, err := pp.RunOnStreamChunk(context.Background(), reqID, &engine.StreamEvent{TextDelta: &text}); err != nil {
		t.Fatal(err)
	}
	if got := pp.InvokedPlugins(reqID); !slices.Equal(got, []string{"test-stream-fanout", "test-stream-delay-stop", "test-stream-mutator"}) {
		t.Fatalf("dispatch order = %v", got)
	}
}

func TestTickDispatchUsesHookOrder(t *testing.T) {
	const fixtures = "../../examples/plugins"
	manifest, err := os.ReadFile(fixtures + "/test-ticker/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	wasmBytes, err := os.ReadFile(fixtures + "/test-ticker/plugin.wasm")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	order := []string{"tick-a", "tick-b", "tick-c"}
	for _, name := range order {
		pluginDir := filepath.Join(dir, name)
		if err := os.MkdirAll(pluginDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(strings.ReplaceAll(string(manifest), "test-ticker", name)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pluginDir, "plugin.wasm"), wasmBytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { _ = rt.Close() })
	pp, err := NewPipeline(rt, PluginConfig{
		Dir:             dir,
		Order:           order,
		HookOrder:       map[string][]string{"run_on_tick": {"tick-c", "tick-a", "tick-b"}},
		AllowUnapproved: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	outcomes := pp.RunOnTick(context.Background(), 994, tickRequest(2))
	got := make([]string, len(outcomes))
	for i, outcome := range outcomes {
		got[i] = outcome.Plugin
	}
	if !slices.Equal(got, []string{"tick-c", "tick-a", "tick-b"}) {
		t.Fatalf("tick order = %v", got)
	}
}
