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

func TestValidateApprovalBindsDigestAndRequestedPermissions(t *testing.T) {
	bundle := PluginBundle{
		Digest: "sha256:installed",
		Manifest: PluginManifest{
			FailureMode: "block",
			Permissions: []Permission{{Name: "env.log"}},
		},
	}

	if _, _, err := validateApproval(bundle, Approval{
		Digest: "sha256:other",
	}); err == nil {
		t.Fatal("digest mismatch was accepted")
	}
	if _, _, err := validateApproval(bundle, Approval{
		Digest:      bundle.Digest,
		Permissions: []string{"env.route_request"},
	}); err == nil {
		t.Fatal("unrequested permission was accepted")
	}

	grants, failureMode, err := validateApproval(bundle, Approval{
		Digest:      bundle.Digest,
		Permissions: []string{"env.log"},
	})
	if err != nil {
		t.Fatalf("valid approval: %v", err)
	}
	if len(grants) != 1 || grants[0] != "env.log" {
		t.Fatalf("grants = %v", grants)
	}
	if failureMode != "block" {
		t.Fatalf("failure mode = %q, want manifest recommendation block", failureMode)
	}
}

func TestValidateApprovalAllowsOperatorFailureModeOverride(t *testing.T) {
	bundle := PluginBundle{
		Digest: "sha256:installed",
		Manifest: PluginManifest{
			FailureMode: "block",
		},
	}
	_, failureMode, err := validateApproval(bundle, Approval{
		Digest:      bundle.Digest,
		FailureMode: "pass",
	})
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if failureMode != "pass" {
		t.Fatalf("failure mode = %q, want operator override pass", failureMode)
	}
}

func TestBundleDigestCoversCodeAndPolicyFiles(t *testing.T) {
	base := bundleDigest([]byte(`{"failure_mode":"pass"}`), []byte("wasm"), []byte(`{"type":"object"}`))
	cases := []string{
		bundleDigest([]byte(`{"failure_mode":"block"}`), []byte("wasm"), []byte(`{"type":"object"}`)),
		bundleDigest([]byte(`{"failure_mode":"pass"}`), []byte("wasm2"), []byte(`{"type":"object"}`)),
		bundleDigest([]byte(`{"failure_mode":"pass"}`), []byte("wasm"), []byte(`{"type":"string"}`)),
	}
	for _, changed := range cases {
		if changed == base {
			t.Fatal("bundle digest did not change when a consumed file changed")
		}
	}
}

func TestValidateManifestContract(t *testing.T) {
	valid := PluginManifest{
		SchemaVersion:        1,
		ID:                   "local/example",
		Name:                 "example",
		Version:              "0.1.0",
		ABIVersion:           "v1",
		MinimumToranaVersion: "0.1.0",
		FailureMode:          "pass",
		Hooks:                []Hook{{Name: "run_before_request"}},
		Permissions:          []Permission{{Name: "env.log"}},
	}
	if err := validateManifest(valid); err != nil {
		t.Fatalf("valid manifest: %v", err)
	}
	invalid := valid
	invalid.ABIVersion = "v2"
	if err := validateManifest(invalid); err == nil {
		t.Fatal("unsupported ABI was accepted")
	}
	invalid = valid
	invalid.Permissions = []Permission{{Name: "env.not_real"}}
	if err := validateManifest(invalid); err == nil {
		t.Fatal("unknown permission was accepted")
	}
	invalid = valid
	invalid.MinimumToranaVersion = "99.0.0"
	if err := validateManifest(invalid); err == nil {
		t.Fatal("incompatible minimum host version was accepted")
	}
}

func TestExternalBundlesConformToCurrentHost(t *testing.T) {
	root := os.Getenv("TORANA_PLUGIN_BUNDLES_DIR")
	if root == "" {
		t.Skip("TORANA_PLUGIN_BUNDLES_DIR is unset")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read bundles: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			runtime := wasm.NewRuntime(context.Background())
			defer runtime.Close()
			pipeline, err := NewPipeline(runtime, PluginConfig{
				Dir:             root,
				Order:           []string{entry.Name()},
				AllowUnapproved: true,
				Strict:          true,
			})
			if err != nil {
				t.Fatalf("host conformance: %v", err)
			}
			if pipeline.Len() != 1 {
				t.Fatalf("loaded plugins = %d, want 1", pipeline.Len())
			}
		})
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
		Dir:             "../../plugins",
		Order:           []string{"intent"},
		AllowUnapproved: true,
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
