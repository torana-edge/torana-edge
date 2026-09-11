package plugin

import (
	"context"
	"testing"

	"github.com/torana-edge/torana-edge/internal/wasm"
)

// TestUnapprovedPluginSkippedNotFatal pins the fix for the upgrade dead end.
//
// A managed config store written by any pre-approval build carries
// plugins.order forward with an empty approvals map. Strict mode used to
// treat that as a configuration error and abort the whole reload, which the
// control plane surfaces as a rejected save — so an operator could not change
// an unrelated setting while any stale entry sat in plugins.order, and the
// error text named a digest without saying approval was a UI action.
//
// The security property is unchanged and asserted here: the unapproved
// plugin's code must not be loaded. What changes is that the rest of the
// configuration still applies, and the skip is reported rather than fatal.
func TestUnapprovedPluginSkippedNotFatal(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")

	ctx := context.Background()
	runtime := wasm.NewRuntime(ctx)
	defer runtime.Close()

	// Strict, enabled, and deliberately unapproved — the exact shape of a
	// store carried over from a build that predates digest-bound approvals.
	pipeline, err := NewPipeline(runtime, PluginConfig{
		Dir:       fixturesDir,
		Order:     []string{"test-mutator"},
		Approvals: nil,
		Strict:    true,
	})
	if err != nil {
		t.Fatalf("a missing approval must not fail the reload, got: %v", err)
	}
	defer pipeline.DrainAndClose()

	if pipeline.Len() != 0 {
		t.Fatalf("unapproved plugin was loaded (Len=%d) — approval enforcement is broken", pipeline.Len())
	}

	skipped := pipeline.Skipped()
	if len(skipped) != 1 {
		t.Fatalf("expected exactly 1 reported skip, got %d: %+v", len(skipped), skipped)
	}
	if skipped[0].Name != "test-mutator" {
		t.Errorf("skip names %q, want test-mutator", skipped[0].Name)
	}
	if skipped[0].Digest == "" {
		t.Error("skip must carry the digest the operator needs to approve")
	}
	if skipped[0].Reason == "" {
		t.Error("skip must carry a human-readable reason")
	}
}

// TestApprovedPluginStillLoads guards the other direction: the skip path must
// not swallow plugins that are correctly approved.
func TestApprovedPluginStillLoads(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	digest, err := BundleDigestForDir(fixturesDir + "/test-mutator")
	if err != nil {
		t.Fatalf("BundleDigestForDir: %v", err)
	}
	permissions := []string{
		"env.log",
		"ir.messages.write.user",
		"ir.messages.write.assistant",
		"ir.messages.write.system",
		"ir.messages.write.tool",
		"ir.messages.write.developer",
		"ir.messages.write.other",
		"ir.tools.write",
		"ir.model.write",
		"ir.params.write",
		"ir.cache_control.write",
	}

	ctx := context.Background()
	runtime := wasm.NewRuntime(ctx)
	defer runtime.Close()

	pipeline, err := NewPipeline(runtime, PluginConfig{
		Dir:   fixturesDir,
		Order: []string{"test-mutator"},
		Approvals: map[string]Approval{
			"test-mutator": {Digest: digest, Permissions: permissions, FailureMode: "pass"},
		},
		Strict: true,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	defer pipeline.DrainAndClose()

	if pipeline.Len() != 1 {
		t.Fatalf("approved plugin failed to load (Len=%d)", pipeline.Len())
	}
	if got := len(pipeline.Skipped()); got != 0 {
		t.Errorf("approved plugin reported as skipped: %+v", pipeline.Skipped())
	}
}
