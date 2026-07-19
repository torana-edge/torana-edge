package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

func TestHasHook_MatchAfterManifestFix(t *testing.T) {
	m := PluginManifest{
		Name: "test-plugin",
		Hooks: []Hook{
			{Name: "run_before_request", Priority: 100},
		},
	}
	if !hasHook(m, "run_before_request") {
		t.Error("hasHook should match run_before_request after manifest fix")
	}
	if hasHook(m, "on_chat_request") {
		t.Error("hasHook should NOT match on_chat_request — manifests were updated")
	}
}

func TestHasHook_MultipleHooks(t *testing.T) {
	m := PluginManifest{
		Name: "multi-hook",
		Hooks: []Hook{
			{Name: "run_before_request", Priority: 100},
			{Name: "run_after_response", Priority: 200},
			{Name: "run_on_stream_chunk", Priority: 300},
		},
	}
	for _, h := range []string{"run_before_request", "run_after_response", "run_on_stream_chunk"} {
		if !hasHook(m, h) {
			t.Errorf("hasHook should match %s", h)
		}
	}
	if hasHook(m, "on_chat_request") {
		t.Error("hasHook should NOT match on_chat_request")
	}
}

func TestHookNames(t *testing.T) {
	hooks := []Hook{
		{Name: "run_before_request", Priority: 100},
		{Name: "run_after_response", Priority: 200},
	}
	names := hookNames(hooks)
	if len(names) != 2 {
		t.Fatalf("expected 2 hook names, got %d", len(names))
	}
	if names[0] != "run_before_request" || names[1] != "run_after_response" {
		t.Errorf("unexpected hook names: %v", names)
	}
}

func TestDiscoverPlugins_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	bundles, err := DiscoverPlugins(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 0 {
		t.Errorf("expected 0 bundles in empty dir, got %d", len(bundles))
	}
}

func TestPipelineDrainRejectsNewRequestsUntilPinnedWorkFinishes(t *testing.T) {
	rt := wasm.NewRuntime(context.Background())
	pp, err := NewPipeline(rt, PluginConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if !pp.TryAcquire() {
		t.Fatal("initial request was not admitted")
	}

	drained := make(chan struct{})
	go func() {
		pp.DrainAndClose()
		close(drained)
	}()

	deadline := time.After(time.Second)
	for !isDraining(pp) {
		select {
		case <-deadline:
			t.Fatal("pipeline never entered draining state")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if pp.TryAcquire() {
		t.Fatal("draining pipeline admitted a new request")
	}
	select {
	case <-drained:
		t.Fatal("pipeline closed while a request was pinned")
	default:
	}

	pp.Release()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("pipeline did not close after pinned work finished")
	}
}

func isDraining(pp *PluginPipeline) bool {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	return pp.draining
}

func TestDiscoverPlugins_ValidPlugin(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "test-plugin")
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := PluginManifest{
		Name:        "test-plugin",
		Version:     "0.1.0",
		Description: "test",
		Hooks: []Hook{
			{Name: "run_before_request", Priority: 100},
		},
	}
	mBytes, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), mBytes, 0644); err != nil {
		t.Fatal(err)
	}
	// Write a minimal valid WASM file (magic bytes + version).
	// This won't execute, but DiscoverPlugins only reads the file — it doesn't instantiate.
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.wasm"), wasmBytes, 0644); err != nil {
		t.Fatal(err)
	}
	bundles, err := DiscoverPlugins(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 1 {
		t.Fatalf("expected 1 bundle, got %d", len(bundles))
	}
	if bundles[0].Manifest.Name != "test-plugin" {
		t.Errorf("unexpected plugin name: %s", bundles[0].Manifest.Name)
	}
}

// TestPipelineRunBeforeRequest exercises the full dispatch path (manifest → discovery →
// pipeline → hook call) using a real compiled WASM plugin, rather than calling
// CallRequest directly. This catches manifest/dispatch mismatches that the
// existing direct-call tests miss.
func TestPipelineRunBeforeRequest_FullDispatch(t *testing.T) {
	requireWASM(t, "../../plugins/intent/plugin.wasm")

	ctx := context.Background()
	runtime := wasm.NewRuntime(ctx)
	defer runtime.Close()

	pipeline, err := NewPipeline(runtime, PluginConfig{
		Dir:   "../../plugins",
		Order: []string{"intent"},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if pipeline.Len() != 1 {
		t.Fatalf("intent plugin not loaded (loaded=%d)", pipeline.Len())
	}

	chat := &engine.ChatRequest{
		Messages: []engine.Message{{Role: engine.RoleUser, Content: "hi"}},
		Tools: []engine.ToolDef{{
			Name: "read",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
			},
		}},
	}
	result, err := pipeline.RunBeforeRequest(ctx, 1, chat)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}

	// The intent plugin injects the "i" intent field into tool schemas
	// via the full dispatch path.
	if len(result.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result.Tools))
	}
	props, _ := result.Tools[0].Parameters["properties"].(map[string]any)
	if _, ok := props["i"]; !ok {
		t.Errorf(`expected "i" injected into tool schema via full dispatch path, got %v`, result.Tools[0].Parameters)
	}
}
