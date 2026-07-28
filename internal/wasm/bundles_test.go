package wasm

import (
	"os"
	"testing"
)

// See internal/plugin/bundles_test.go for the reasoning. This package's ABI
// tests use fixtures; the one test that asserts the official plugins load
// against the current host is a conformance check and runs from torana-plugins
// CI, which builds those bundles.

const fixturesDir = "../../examples/plugins"

func officialBundlesDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("TORANA_PLUGIN_BUNDLES_DIR")
	if dir == "" {
		t.Skip("TORANA_PLUGIN_BUNDLES_DIR unset — official-plugin conformance is verified from torana-plugins CI")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("TORANA_PLUGIN_BUNDLES_DIR=%q is not readable: %v", dir, err)
	}
	// Marker consumed by torana-plugins CI to prove this suite actually ran.
	// A gate that silently skipped everywhere would look identical to a green
	// run, and that is the failure mode this split introduces.
	t.Logf("official-plugin behaviour: bundles from %s", dir)
	return dir
}
