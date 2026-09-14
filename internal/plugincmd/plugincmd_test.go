package plugincmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
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
		t.Fatalf("manifest schema/failure fields missing:\n%s", manifest)
	}
}

func TestScaffoldBuildsFromCleanSourceWithoutMutatingModuleFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "clean-plugin")
	var stdout, stderr bytes.Buffer
	if err := Run([]string{"plugin", "init", dir}, &stdout, &stderr); err != nil {
		t.Fatalf("init: %v", err)
	}
	goModPath := filepath.Join(dir, "go.mod")
	goMod, err := os.ReadFile(goModPath)
	if err != nil {
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
	sumPath := filepath.Join(dir, "go.sum")
	sum, err := os.ReadFile(sumPath)
	if err != nil {
		t.Fatalf("init did not create go.sum: %v", err)
	}
	after, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(goMod) {
		t.Fatal("clean-room build modified source go.mod")
	}
	afterSum, err := os.ReadFile(sumPath)
	if err != nil || string(afterSum) != string(sum) {
		t.Fatalf("clean-room build modified source go.sum: %v", err)
	}
}

// TestScaffoldFirstRunAgainstStagedSDK executes the generated project's real
// native test and WASI build. The replacement is installed only in this temp
// project; no unreleased local path can leak into the scaffold a user creates.
func TestScaffoldFirstRunAgainstStagedSDK(t *testing.T) {
	staged := os.Getenv("TORANA_SDK_DIR")
	if staged == "" {
		t.Skip("TORANA_SDK_DIR is not set")
	}
	if _, err := os.Stat(filepath.Join(staged, "go.mod")); err != nil {
		t.Skipf("staged SDK unavailable: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "first-plugin")
	var stdout, stderr bytes.Buffer
	if err := Run([]string{"plugin", "new", dir}, &stdout, &stderr); err != nil {
		t.Fatalf("new: %v\nstderr: %s", err, stderr.String())
	}
	edit := exec.Command("go", "mod", "edit", "-replace", "github.com/torana-edge/torana-plugin-sdk="+staged)
	edit.Dir = dir
	if output, err := edit.CombinedOutput(); err != nil {
		t.Fatalf("stage SDK replacement: %v\n%s", err, output)
	}
	goTest := exec.Command("go", "test", "./...")
	goTest.Dir = dir
	if output, err := goTest.CombinedOutput(); err != nil {
		t.Fatalf("generated native test: %v\n%s", err, output)
	}
	wasm := exec.Command("go", "build", "-o", filepath.Join(dir, "plugin.wasm"), ".")
	wasm.Dir = dir
	wasm.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0", "GOWORK=off")
	if output, err := wasm.CombinedOutput(); err != nil {
		t.Fatalf("generated WASI build: %v\n%s", err, output)
	}
}

func TestRustScaffoldUsesTypedHookInNativeUnitTest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rust-plugin")
	var stdout, stderr bytes.Buffer
	if err := Run([]string{"plugin", "new", dir, "--language", "rust"}, &stdout, &stderr); err != nil {
		t.Fatalf("new rust plugin: %v\nstderr: %s", err, stderr.String())
	}
	source, err := os.ReadFile(filepath.Join(dir, "src", "lib.rs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<PluginImpl as Plugin>::before_request",
		"result.into_hook_result().is_none()",
		"info(&format!",
	} {
		if !strings.Contains(string(source), want) {
			t.Fatalf("generated Rust unit test lacks %q:\n%s", want, source)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "tests", "native.rs")); !os.IsNotExist(err) {
		t.Fatalf("placeholder integration test still exists: %v", err)
	}
	cargo, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cargo), scaffoldRustSDKDependency()) {
		t.Fatalf("generated Cargo dependency is not the exact SDK Git revision:\n%s", cargo)
	}
}

func TestRustScaffoldFirstRunAgainstStagedSDK(t *testing.T) {
	sdkRoot := os.Getenv("TORANA_SDK_DIR")
	if sdkRoot == "" {
		t.Skip("TORANA_SDK_DIR is not set")
	}
	staged := filepath.Join(sdkRoot, "rust", "torana-plugin-sdk")
	if _, err := os.Stat(filepath.Join(staged, "Cargo.toml")); err != nil {
		t.Skipf("staged Rust SDK unavailable: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "first-rust-plugin")
	var stdout, stderr bytes.Buffer
	if err := Run([]string{"plugin", "new", dir, "--language", "rust"}, &stdout, &stderr); err != nil {
		t.Fatalf("new rust plugin: %v\nstderr: %s", err, stderr.String())
	}
	cargoPath := filepath.Join(dir, "Cargo.toml")
	cargo, err := os.ReadFile(cargoPath)
	if err != nil {
		t.Fatal(err)
	}
	localDependency := fmt.Sprintf("torana-plugin-sdk = { path = %q }", staged)
	replaced := strings.Replace(string(cargo), scaffoldRustSDKDependency(), localDependency, 1)
	if replaced == string(cargo) {
		t.Fatalf("generated Cargo manifest lacks dependency to override:\n%s", cargo)
	}
	cargo = []byte(replaced)
	if err := os.WriteFile(cargoPath, cargo, 0o600); err != nil {
		t.Fatal(err)
	}
	targetDir := filepath.Join(t.TempDir(), "cargo-target")
	for _, args := range [][]string{{"test"}, {"build", "--release", "--target", "wasm32-wasip1"}} {
		cmd := exec.Command("cargo", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "CARGO_TARGET_DIR="+targetDir)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generated Rust cargo %s: %v\n%s", strings.Join(args, " "), err, output)
		}
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
	sdkRoot := os.Getenv("TORANA_SDK_DIR")
	if sdkRoot == "" {
		sdkRoot = "../../../torana-plugin-sdk"
	}
	sdkGoMod := filepath.Join(sdkRoot, "go.mod")
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

// sdkModulePath now lives in lint.go, which needs it to resolve the SDK import
// alias. One definition, so the test cannot drift from what the linter matches.

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

func TestInitDependencyFailureLeavesDestinationUntouched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\necho unavailable >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, existed := range []bool{false, true} {
		parent := t.TempDir()
		dir := filepath.Join(parent, "plugin")
		if existed {
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		var out bytes.Buffer
		err := initPlugin([]string{dir}, &out)
		if err == nil || !strings.Contains(err.Error(), "resolve scaffold dependencies") {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.Len() != 0 {
			t.Fatalf("reported success: %s", out.String())
		}
		if existed {
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 0 {
				t.Fatalf("changed empty target: %v %v", files, err)
			}
		} else if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("left partial destination: %v", err)
		}
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if existed {
			want = 1
		}
		if len(entries) != want {
			t.Fatalf("left staging files: %v", entries)
		}
	}
}

func TestInitPreservesExistingFiles(t *testing.T) {
	for _, language := range []string{"go", "rust"} {
		dir := t.TempDir()
		original := []byte("user-owned content")
		path := filepath.Join(dir, "plugin.json")
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Fatal(err)
		}
		err := initPlugin([]string{dir, "--language", language}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "not empty") {
			t.Fatalf("accepted nonempty project: %v", err)
		}
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, original) {
			t.Fatalf("overwrote user data: %s %v", actual, err)
		}
	}
}

func TestScaffoldRustRevisionMatchesHostModule(t *testing.T) {
	cmd := exec.Command("go", "mod", "download", "-json", sdkModulePath+"@"+requireVersionFromGoMod(t, "../../go.mod", sdkModulePath))
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("resolve host SDK source: %v", err)
	}
	var downloaded struct{ Origin struct{ Hash string } }
	if err := json.Unmarshal(output, &downloaded); err != nil {
		t.Fatal(err)
	}
	if downloaded.Origin.Hash != ScaffoldSDKRevision {
		t.Fatalf("Rust scaffold revision %s differs from host module source %s", ScaffoldSDKRevision, downloaded.Origin.Hash)
	}
}

// This gate runs in the dedicated Rust conformance CI job. It proves the
// generated Git pin can be resolved without a local SDK or registry release.
func TestRustScaffoldFirstRunAgainstPinnedSDK(t *testing.T) {
	if os.Getenv("TORANA_RUST_CONFORMANCE") != "1" {
		t.Skip("requires Rust conformance toolchain")
	}
	dir := filepath.Join(t.TempDir(), "pinned-rust-plugin")
	if err := initPlugin([]string{dir, "--language", "rust"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"test"}, {"build", "--release", "--target", "wasm32-wasip1"}} {
		cmd := exec.Command("cargo", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "CARGO_TARGET_DIR="+filepath.Join(dir, "target"))
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("pinned scaffold cargo %s: %v\n%s", strings.Join(args, " "), err, output)
		}
	}
}
