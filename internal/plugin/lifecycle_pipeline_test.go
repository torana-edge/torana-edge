package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/torana-edge/torana-edge/internal/wasm"
)

// TestPipelineRejectionUnloadsPlugin — row 7: a post-load rejection in
// NON-strict mode must unload the plugin from the runtime (reachability
// removed, resources released exactly once), so the name can be loaded again.
// Strict whole-pipeline failure remains the caller's runtime-close concern.
func TestPipelineRejectionUnloadsPlugin(t *testing.T) {
	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { _ = rt.Close() })

	// A temp bundle dir: copy the test-inert-a fixture but declare a hook the
	// guest does not export, so ValidateHooks rejects it AFTER LoadPlugin.
	src := filepath.Join("..", "..", "examples", "plugins", "test-inert-a")
	srcWasm, err := os.ReadFile(filepath.Join(src, "plugin.wasm"))
	if err != nil {
		t.Fatalf("fixture not built — run make testdata: %v", err)
	}
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "test-inert-a")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{
	  "schema_version": 1,
	  "id": "torana-test/test-inert-a",
	  "name": "test-inert-a",
	  "version": "0.1.0",
	  "abi_version": "v2",
	  "failure_mode": "pass",
	  "hooks": [{"name": "run_before_request"}, {"name": "run_on_tick"}],
	  "permissions": []
	}`
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.wasm"), srcWasm, 0o644); err != nil {
		t.Fatal(err)
	}

	// Non-strict: the pipeline succeeds and skips the rejected plugin.
	pp, err := NewPipeline(rt, PluginConfig{Dir: dir, Order: []string{"test-inert-a"}, Strict: false})
	if err != nil {
		t.Fatalf("non-strict pipeline with a rejected plugin: %v", err)
	}
	if len(pp.Skipped()) == 0 {
		t.Fatal("expected the hook-mismatched plugin to be skipped")
	}

	// Reachability was removed: the same name loads cleanly again.
	if _, err := rt.LoadPlugin("test-inert-a", srcWasm); err != nil {
		t.Fatalf("reload after non-strict rejection failed — the plugin was not unloaded: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
}
