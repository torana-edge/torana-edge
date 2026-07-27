package plugincmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitCreatesStandaloneSDKPlugin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "redactor")
	var stdout, stderr bytes.Buffer
	if err := Run([]string{"plugin", "init", dir}, &stdout, &stderr); err != nil {
		t.Fatalf("Run: %v\nstderr: %s", err, stderr.String())
	}
	for _, name := range []string{"go.mod", "plugin.wasm.go", "plugin.json", "schema.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was not created: %v", name, err)
		}
	}
	goMod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	// Asserted against the constant the scaffold uses, not a copy of the
	// string. The copy meant this test locked in whatever the scaffold said:
	// it named v0.1.0 long after v0.1.3 shipped, and stayed green.
	wantSDK := "github.com/torana-edge/torana-plugin-sdk " + ScaffoldSDKVersion
	if !strings.Contains(string(goMod), wantSDK) {
		t.Fatalf("standalone SDK dependency missing %q:\n%s", wantSDK, goMod)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), `"schema_version": 1`) ||
		!strings.Contains(string(manifest), `"failure_mode": "pass"`) {
		t.Fatalf("v1 manifest fields missing:\n%s", manifest)
	}
}

func TestRunRejectsUnknownPluginCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run([]string{"plugin", "publish"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "unknown plugin command") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(stderr.String(), "torana plugin init") {
		t.Fatalf("usage missing: %s", stderr.String())
	}
}

// TestScaffoldSDKVersionMatchesTheHost anchors the scaffold to something other
// than itself.
//
// Asserting the scaffold against its own constant is circular: change the
// constant and the test follows. What makes the version WRONG is disagreeing
// with reality — so this compares it to the SDK version torana-edge is built
// against, read from go.mod. A plugin scaffolded against a different SDK than
// the host implements is the exact drift that produced "v0.1.0" surviving three
// SDK releases.
func TestScaffoldSDKVersionMatchesTheHost(t *testing.T) {
	goMod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	const dep = "github.com/torana-edge/torana-plugin-sdk "
	i := strings.Index(string(goMod), dep)
	if i < 0 {
		t.Fatal("torana-edge does not depend on the plugin SDK")
	}
	rest := string(goMod)[i+len(dep):]
	hostVersion := strings.Fields(rest)[0]

	if ScaffoldSDKVersion != hostVersion {
		t.Errorf("torana plugin new scaffolds SDK %s, but torana-edge is built against %s.\n"+
			"A plugin scaffolded against a different SDK than the host implements can fail in "+
			"ways the author cannot debug. Update ScaffoldSDKVersion when bumping the SDK.",
			ScaffoldSDKVersion, hostVersion)
	}
}
