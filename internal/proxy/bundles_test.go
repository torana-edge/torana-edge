package proxy

import (
	"os"
	"testing"
)

// See internal/plugin/bundles_test.go for why this split exists. In short:
// host mechanics are tested against the fixtures in examples/plugins/, and
// assertions about a real plugin's behaviour run from torana-plugins CI against
// bundles built by the repo that owns them.

const fixturesDir = "../../examples/plugins"

func officialBundlesDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("TORANA_PLUGIN_BUNDLES_DIR")
	if dir == "" {
		t.Skip("TORANA_PLUGIN_BUNDLES_DIR unset — official-plugin behaviour is verified from torana-plugins CI")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("TORANA_PLUGIN_BUNDLES_DIR=%q is not readable: %v", dir, err)
	}
	return dir
}

func requireBundle(t *testing.T, dir, name string) {
	t.Helper()
	if _, err := os.Stat(dir + "/" + name + "/plugin.wasm"); err != nil {
		t.Skipf("bundle %q not present in %s", name, dir)
	}
}
