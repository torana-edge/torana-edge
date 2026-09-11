package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	  "abi_version": "v1",
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
	digest, err := BundleDigestForDir(pluginDir)
	if err != nil {
		t.Fatal(err)
	}

	// Non-strict: the pipeline succeeds and skips the rejected plugin.
	pp, err := NewPipeline(rt, PluginConfig{
		Dir: dir, Order: []string{"test-inert-a"}, Strict: false,
		Approvals: map[string]Approval{
			"torana-test/test-inert-a": {Digest: digest},
		},
	})
	if err != nil {
		t.Fatalf("non-strict pipeline with a rejected plugin: %v", err)
	}
	if pp.Len() != 0 {
		t.Fatalf("loaded %d plugins, want hook-mismatched plugin rejected", pp.Len())
	}
	if len(pp.Skipped()) != 0 {
		t.Fatalf("plugin was rejected before post-load hook validation: %+v", pp.Skipped())
	}

	// Reachability was removed: the same name loads cleanly again.
	reloaded, err := rt.LoadPlugin("test-inert-a", srcWasm)
	if err != nil {
		t.Fatalf("reload after non-strict rejection failed — the plugin was not unloaded: %v", err)
	}
	declared, err := manifestHooks([]Hook{{Name: "run_before_request"}, {Name: "run_on_tick"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.ValidateHooks(context.Background(), declared); err == nil ||
		!strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "HOOK_ON_TICK") {
		t.Fatalf("hook validation error = %v, want missing HOOK_ON_TICK", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
}
