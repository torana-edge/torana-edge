package plugincmd

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
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

func TestScaffoldBuildsFromCleanSourceWithoutMutatingModuleFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "clean-plugin")
	var stdout, stderr bytes.Buffer
	if err := Run([]string{"plugin", "init", dir}, &stdout, &stderr); err != nil {
		t.Fatalf("init: %v", err)
	}
	sdkDir, err := filepath.Abs("../../../torana-plugin-sdk")
	if err != nil {
		t.Fatal(err)
	}
	goModPath := filepath.Join(dir, "go.mod")
	goMod, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatal(err)
	}
	goMod = append(goMod, []byte("\nreplace "+sdkModulePath+" => "+sdkDir+"\n")...)
	if err := os.WriteFile(goModPath, goMod, 0o644); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	if err := Run([]string{"plugin", "build", dir}, &stdout, &stderr); err != nil {
		t.Fatalf("build scaffold: %v\nstderr: %s", err, stderr.String())
	}
	if info, err := os.Stat(filepath.Join(dir, "plugin.wasm")); err != nil || info.Size() == 0 {
		t.Fatalf("plugin.wasm was not built: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.sum")); !os.IsNotExist(err) {
		t.Fatalf("clean-room build unexpectedly modified source go.sum: %v", err)
	}
	after, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(goMod) {
		t.Fatal("clean-room build modified source go.mod")
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
	hostVersion := requireVersionFromGoMod(t, "../../go.mod", sdkModulePath)
	if ScaffoldSDKVersion != hostVersion {
		t.Errorf("torana plugin new scaffolds SDK %s, but torana-edge is built against %s.\n"+
			"A plugin scaffolded against a different SDK than the host implements can fail in "+
			"ways the author cannot debug. Update ScaffoldSDKVersion when bumping the SDK.",
			ScaffoldSDKVersion, hostVersion)
	}
}

// TestScaffoldGoVersionSatisfiesTheSDK anchors the other constant. A scaffolded
// module declaring an older Go version than the SDK requires does not build,
// and nothing previously asserted it at all.
func TestScaffoldGoVersionSatisfiesTheSDK(t *testing.T) {
	const sdkGoMod = "../../../torana-plugin-sdk/go.mod"
	raw, err := os.ReadFile(sdkGoMod)
	if err != nil {
		t.Skip("torana-plugin-sdk not checked out beside this repo")
	}
	m := regexp.MustCompile(`(?m)^go\s+(\S+)`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("no go directive in the SDK go.mod")
	}
	if scaffoldGoVersion != m[1] {
		t.Errorf("the scaffold writes `go %s` but the SDK requires `go %s`. A scaffolded "+
			"module below its dependency's requirement does not build.", scaffoldGoVersion, m[1])
	}
}

// TestRequireVersionIgnoresReplaceDirectives — the first version of this
// helper took the first occurrence of the module path and the next field, so a
// replace directive above the require block yielded "=>" as the host version.
func TestRequireVersionIgnoresReplaceDirectives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	content := "module example.com/x\n\ngo 1.25.0\n\n" +
		"replace " + sdkModulePath + " => ../torana-plugin-sdk\n\n" +
		"require (\n\t" + sdkModulePath + " v0.1.3\n)\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := requireVersionFromGoMod(t, path, sdkModulePath); got != "v0.1.3" {
		t.Errorf("got %q, want v0.1.3 — a replace directive must not be read as the version", got)
	}
}

const sdkModulePath = "github.com/torana-edge/torana-plugin-sdk"

// requireVersionFromGoMod reads a module's REQUIRED version.
//
// The first version of this took the first strings.Index of the module path and
// the next field, which a `replace github.com/... => ../local` line above the
// require block turns into "=>". Anchoring on a version token avoids that, and
// replace/exclude lines are skipped explicitly.
func requireVersionFromGoMod(t *testing.T, path, module string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "replace ") || strings.HasPrefix(trimmed, "exclude ") ||
			strings.HasPrefix(trimmed, "//") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(trimmed, "require "))
		if len(fields) >= 2 && fields[0] == module && strings.HasPrefix(fields[1], "v") {
			return fields[1]
		}
	}
	t.Fatalf("%s does not require %s", path, module)
	return ""
}
