package plugin

import (
	"os"
	"testing"
)

// Two kinds of test live in this repository, and they resolve their plugins
// differently on purpose.
//
// **Host mechanics** — is a declared hook dispatched, is a missing grant
// refused, does a verdict short-circuit the transport — are tested against the
// purpose-built fixtures in examples/plugins/. They belong here because they
// are assertions about the host, and a fixture's output is predictable enough
// to compare byte for byte. A real plugin's is not: pii's depends on regexes
// and a model, otel's histogram values on request timing.
//
// **Official-plugin behaviour** — does pii detect PII, does the warmer stop at
// break-even — is an assertion about the *plugin*. Those tests run against real
// built bundles supplied by torana-plugins CI, which builds them from the repo
// that owns them. They skip here, because torana-edge does not own those
// plugins and should not need a copy of them to test itself.
//
// That split is why this repository's own suite has no dependency on any real
// plugin. Anything that still calls officialBundlesDir is, by definition,
// testing a plugin rather than the host.

// fixturesDir is the purpose-built plugin tree. Always present in this repo.
const fixturesDir = "../../examples/plugins"

// officialBundlesDir returns the directory of built official plugin bundles, or
// skips the test when none was supplied.
//
// torana-plugins CI sets TORANA_PLUGIN_BUNDLES_DIR to its own dist/ after
// building every plugin, so these run there against bundles built from the
// source that owns them.
func officialBundlesDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("TORANA_PLUGIN_BUNDLES_DIR")
	if dir == "" {
		t.Skip("TORANA_PLUGIN_BUNDLES_DIR unset — official-plugin behaviour is verified from torana-plugins CI")
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

// requireBundle skips unless the named official bundle is present in the
// supplied bundle directory.
func requireBundle(t *testing.T, dir, name string) {
	t.Helper()
	if _, err := os.Stat(dir + "/" + name + "/plugin.wasm"); err != nil {
		t.Skipf("bundle %q not present in %s", name, dir)
	}
}
