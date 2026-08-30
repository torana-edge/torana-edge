package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/wasm"
)

func writeConflictBundle(t *testing.T, root, name, id string, conflicts []string, wasmBytes []byte) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := PluginManifest{
		SchemaVersion: 1,
		ID:            id,
		Name:          name,
		Version:       "0.1.0",
		ABIVersion:    "v1",
		FailureMode:   "pass",
		Repository:    "https://github.com/torana-edge/torana-plugins",
		Description:   "conflict contract fixture",
		Hooks:         []Hook{{Name: "run_before_request"}},
		ConflictsWith: conflicts,
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.wasm"), wasmBytes, 0o644); err != nil {
		t.Fatal(err)
	}
}

func conflictApprovals(t *testing.T, root string, names ...string) map[string]Approval {
	t.Helper()
	bundles, err := DiscoverPlugins(root)
	if err != nil {
		t.Fatal(err)
	}
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}
	approvals := make(map[string]Approval, len(names))
	for _, bundle := range bundles {
		if _, ok := wanted[bundle.Manifest.Name]; ok {
			approvals[bundle.Manifest.ID] = Approval{Digest: bundle.Digest}
		}
	}
	if len(approvals) != len(names) {
		t.Fatalf("approvals = %v, want names %v", approvals, names)
	}
	return approvals
}

func TestPluginConflictRejectedBeforeGuestCodeLoads(t *testing.T) {
	root := t.TempDir()
	invalidWASM := []byte("this must never reach the wasm loader")
	writeConflictBundle(t, root, "alpha", "test/alpha", []string{"test/beta"}, invalidWASM)
	// One-way on purpose: safety must not rely on the peer reciprocating.
	writeConflictBundle(t, root, "beta", "test/beta", nil, invalidWASM)
	approvals := conflictApprovals(t, root, "alpha", "beta")

	for _, order := range [][]string{{"alpha", "beta"}, {"beta", "alpha"}} {
		t.Run(strings.Join(order, "-then-"), func(t *testing.T) {
			runtime := wasm.NewRuntime(context.Background())
			defer runtime.Close()
			_, err := NewPipeline(runtime, PluginConfig{
				Dir: root, Order: order, Approvals: approvals, Strict: true,
			})
			if err == nil || !strings.Contains(err.Error(), "plugin conflict") ||
				!strings.Contains(err.Error(), `"alpha" (test/alpha)`) ||
				!strings.Contains(err.Error(), `"beta" (test/beta)`) {
				t.Fatalf("conflict error = %v", err)
			}
			if strings.Contains(err.Error(), "compile") || strings.Contains(err.Error(), "wasm") {
				t.Fatalf("guest code was reached before conflict rejection: %v", err)
			}
		})
	}
}

func TestPluginConflictAppliesToAllowUnapprovedConformancePipelines(t *testing.T) {
	root := t.TempDir()
	invalidWASM := []byte("this must never reach the wasm loader")
	writeConflictBundle(t, root, "alpha", "test/alpha", []string{"test/beta"}, invalidWASM)
	writeConflictBundle(t, root, "beta", "test/beta", nil, invalidWASM)

	runtime := wasm.NewRuntime(context.Background())
	defer runtime.Close()
	_, err := NewPipeline(runtime, PluginConfig{
		Dir: root, Order: []string{"alpha", "beta"}, AllowUnapproved: true, Strict: true,
	})
	if err == nil || !strings.Contains(err.Error(), "plugin conflict") {
		t.Fatalf("AllowUnapproved conflict error = %v", err)
	}
}

func TestUnapprovedConflictDoesNotBlockApprovedPlugin(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-inert-a/plugin.wasm")
	wasmBytes, err := os.ReadFile(fixturesDir + "/test-inert-a/plugin.wasm")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeConflictBundle(t, root, "alpha", "test/alpha", []string{"test/beta"}, wasmBytes)
	writeConflictBundle(t, root, "beta", "test/beta", []string{"test/alpha"}, wasmBytes)

	runtime := wasm.NewRuntime(context.Background())
	defer runtime.Close()
	pipeline, err := NewPipeline(runtime, PluginConfig{
		Dir: root, Order: []string{"alpha", "beta"},
		Approvals: conflictApprovals(t, root, "alpha"), Strict: true,
	})
	if err != nil {
		t.Fatalf("single approved plugin: %v", err)
	}
	defer pipeline.DrainAndClose()
	if pipeline.Len() != 1 {
		t.Fatalf("loaded plugins = %d, want 1", pipeline.Len())
	}
	skipped := pipeline.Skipped()
	if len(skipped) != 1 || skipped[0].Name != "beta" {
		t.Fatalf("skipped = %+v, want only beta", skipped)
	}

	// A later approval makes the proposed generation invalid. Construction
	// fails without mutating or draining the already-serving generation; the
	// caller therefore has nothing new to swap in.
	reloadRuntime := wasm.NewRuntime(context.Background())
	defer reloadRuntime.Close()
	_, err = NewPipeline(reloadRuntime, PluginConfig{
		Dir: root, Order: []string{"alpha", "beta"},
		Approvals: conflictApprovals(t, root, "alpha", "beta"), Strict: true,
	})
	if err == nil || !strings.Contains(err.Error(), "plugin conflict") {
		t.Fatalf("conflicting reload error = %v", err)
	}
	if pipeline.Len() != 1 {
		t.Fatalf("serving generation changed after rejected reload: len=%d", pipeline.Len())
	}
}

func TestApprovalInvalidConflictDoesNotBlockValidPlugin(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-inert-a/plugin.wasm")
	wasmBytes, err := os.ReadFile(fixturesDir + "/test-inert-a/plugin.wasm")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeConflictBundle(t, root, "alpha", "test/alpha", []string{"test/beta"}, wasmBytes)
	writeConflictBundle(t, root, "beta", "test/beta", []string{"test/alpha"}, wasmBytes)
	approvals := conflictApprovals(t, root, "alpha")
	approvals["test/beta"] = Approval{Digest: "sha256:not-the-installed-bundle"}

	runtime := wasm.NewRuntime(context.Background())
	defer runtime.Close()
	pipeline, err := NewPipeline(runtime, PluginConfig{
		Dir: root, Order: []string{"alpha", "beta"}, Approvals: approvals, Strict: false,
	})
	if err != nil {
		t.Fatalf("approval-invalid peer: %v", err)
	}
	defer pipeline.DrainAndClose()
	if pipeline.Len() != 1 {
		t.Fatalf("loaded plugins = %d, want only the validly approved alpha", pipeline.Len())
	}
}

func TestPluginConflictReferenceModel(t *testing.T) {
	alpha := PluginBundle{Manifest: PluginManifest{ID: "test/alpha", Name: "alpha", ConflictsWith: []string{"test/beta"}}}
	beta := PluginBundle{Manifest: PluginManifest{ID: "test/beta", Name: "beta"}}
	gamma := PluginBundle{Manifest: PluginManifest{ID: "test/gamma", Name: "gamma"}}

	if err := validateActivePluginConflicts([]PluginBundle{alpha}); err != nil {
		t.Fatalf("single plugin: %v", err)
	}
	if err := validateActivePluginConflicts([]PluginBundle{alpha, gamma}); err != nil {
		t.Fatalf("unrelated plugins: %v", err)
	}
	if err := validateActivePluginConflicts([]PluginBundle{alpha, beta}); err == nil {
		t.Fatal("one-way declared conflict was accepted")
	}
	if err := validateActivePluginConflicts([]PluginBundle{beta, alpha}); err == nil {
		t.Fatal("reversed active order bypassed a one-way conflict")
	}
}

func TestOfficialCompactorsDeclareExecutableConflict(t *testing.T) {
	root := officialBundlesDir(t)
	requireBundle(t, root, "compactor")
	requireBundle(t, root, "keyword_compactor")

	runtime := wasm.NewRuntime(context.Background())
	defer runtime.Close()
	_, err := NewPipeline(runtime, PluginConfig{
		Dir: root, Order: []string{"compactor", "keyword_compactor"},
		AllowUnapproved: true, Strict: true,
	})
	if err == nil || !strings.Contains(err.Error(), "plugin conflict") ||
		!strings.Contains(err.Error(), "torana/compactor") ||
		!strings.Contains(err.Error(), "torana/keyword_compactor") {
		t.Fatalf("official compactor conflict = %v", err)
	}
}
