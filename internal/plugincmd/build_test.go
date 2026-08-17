package plugincmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectPluginLanguageFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  pluginLanguage
		err   string
	}{
		{name: "go", files: []string{"go.mod"}, want: pluginGo},
		{name: "rust", files: []string{"Cargo.toml"}, want: pluginRust},
		{name: "missing", err: "exactly one"},
		{name: "ambiguous", files: []string{"go.mod", "Cargo.toml"}, err: "both"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("manifest"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := detectPluginLanguage(dir)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want substring %q", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("detectPluginLanguage = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestBuildRustPluginUsesIsolatedTargetAndCopiesOneArtifact(t *testing.T) {
	source := t.TempDir()
	writeTestFile(t, filepath.Join(source, "Cargo.toml"), "[package]\nname='demo'\nversion='0.1.0'\n")
	writeTestFile(t, filepath.Join(source, "plugin.json"), `{"name":"demo","version":"0.1.0"}`)
	writeTestFile(t, filepath.Join(source, "schema.json"), `{"fields":[]}`)

	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "cargo.args")
	rustupRecord := filepath.Join(t.TempDir(), "rustup.pwd")
	fakeCargo := `#!/bin/sh
set -eu
printf '%s\n%s\n' "$*" "$RUSTUP_TOOLCHAIN" > "$FAKE_CARGO_RECORD"
mkdir -p "$CARGO_TARGET_DIR/wasm32-wasip1/release"
printf '\000asm\001\000\000\000rust-fixture' > "$CARGO_TARGET_DIR/wasm32-wasip1/release/demo.wasm"
`
	writeTestFile(t, filepath.Join(bin, "cargo"), fakeCargo)
	if err := os.Chmod(filepath.Join(bin, "cargo"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRustup := `#!/bin/sh
set -eu
pwd > "$FAKE_RUSTUP_RECORD"
printf 'stable-test (default)\n'
`
	writeTestFile(t, filepath.Join(bin, "rustup"), fakeRustup)
	if err := os.Chmod(filepath.Join(bin, "rustup"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CARGO_RECORD", record)
	t.Setenv("FAKE_RUSTUP_RECORD", rustupRecord)

	out := filepath.Join(t.TempDir(), "custom.wasm")
	var stdout, stderr bytes.Buffer
	if err := Run([]string{"plugin", "build", source, "-o", out}, &stdout, &stderr); err != nil {
		t.Fatalf("build Rust plugin: %v\nstderr: %s", err, stderr.String())
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "\x00asm\x01\x00\x00\x00rust-fixture" {
		t.Fatalf("artifact = %q", got)
	}
	args, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "build --release --target wasm32-wasip1\nstable-test\n" {
		t.Fatalf("cargo args = %q", args)
	}
	rustupDir, err := os.ReadFile(rustupRecord)
	if err != nil {
		t.Fatal(err)
	}
	if string(rustupDir) != string(filepath.Separator)+"\n" {
		t.Fatalf("rustup ran in %q, want filesystem root (not plugin source)", rustupDir)
	}
	if !strings.Contains(stdout.String(), "Building WASI plugin") || !strings.Contains(stdout.String(), "Built ") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	for _, generated := range []string{"Cargo.lock", ".torana-cargo-target", "target"} {
		if _, err := os.Stat(filepath.Join(source, generated)); !os.IsNotExist(err) {
			t.Fatalf("source was mutated with %s: %v", generated, err)
		}
	}
}

func TestBuildRustPluginRejectsAmbiguousArtifact(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname='demo'\nversion='0.1.0'\n")
	bin := t.TempDir()
	fakeCargo := `#!/bin/sh
set -eu
mkdir -p "$CARGO_TARGET_DIR/wasm32-wasip1/release"
printf a > "$CARGO_TARGET_DIR/wasm32-wasip1/release/a.wasm"
printf b > "$CARGO_TARGET_DIR/wasm32-wasip1/release/b.wasm"
`
	writeTestFile(t, filepath.Join(bin, "cargo"), fakeCargo)
	if err := os.Chmod(filepath.Join(bin, "cargo"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	err := buildRustPlugin(dir, filepath.Join(t.TempDir(), "plugin.wasm"), false, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "2 top-level WASM artifacts") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildRustPluginRejectsNonWASMArtifact(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname='demo'\nversion='0.1.0'\n")
	bin := t.TempDir()
	fakeCargo := `#!/bin/sh
set -eu
mkdir -p "$CARGO_TARGET_DIR/wasm32-wasip1/release"
printf 'not-wasm' > "$CARGO_TARGET_DIR/wasm32-wasip1/release/demo.wasm"
`
	writeTestFile(t, filepath.Join(bin, "cargo"), fakeCargo)
	if err := os.Chmod(filepath.Join(bin, "cargo"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	err := buildRustPlugin(dir, filepath.Join(t.TempDir(), "plugin.wasm"), false, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "not a WebAssembly module") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildRustPluginLockedModeReachesCargo(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname='demo'\nversion='0.1.0'\n")
	writeTestFile(t, filepath.Join(dir, "Cargo.lock"), "# reviewed\n")
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "args")
	fakeCargo := `#!/bin/sh
set -eu
printf '%s\n' "$*" > "$FAKE_CARGO_RECORD"
mkdir -p "$CARGO_TARGET_DIR/wasm32-wasip1/release"
printf '\000asm\001\000\000\000locked' > "$CARGO_TARGET_DIR/wasm32-wasip1/release/demo.wasm"
`
	writeTestFile(t, filepath.Join(bin, "cargo"), fakeCargo)
	if err := os.Chmod(filepath.Join(bin, "cargo"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CARGO_RECORD", record)
	if err := buildRustPlugin(dir, filepath.Join(t.TempDir(), "plugin.wasm"), true, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "build --release --target wasm32-wasip1 --locked\n" {
		t.Fatalf("cargo args = %q", args)
	}
}

func TestCopyTreeSkipsToolchainOutput(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(src, "Cargo.toml"), "source")
	for _, dir := range []string{"target", ".torana-cargo-target", ".git"} {
		writeTestFile(t, filepath.Join(src, dir, "large-output"), "not source")
	}
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "Cargo.toml")); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"target", ".torana-cargo-target", ".git"} {
		if _, err := os.Stat(filepath.Join(dst, dir)); !os.IsNotExist(err) {
			t.Errorf("%s was copied: %v", dir, err)
		}
	}
}

func TestReplaceEnvHasOneAuthoritativeValue(t *testing.T) {
	got := replaceEnv([]string{"A=1", "CARGO_TARGET_DIR=old", "CARGO_TARGET_DIR=older"}, "CARGO_TARGET_DIR", "new")
	if strings.Join(got, "|") != "A=1|CARGO_TARGET_DIR=new" {
		t.Fatalf("env = %v", got)
	}
}

func TestRemoteRustBuildPolicyRequiresReviewedLocalSource(t *testing.T) {
	remote := source{repoURL: "https://example.test/plugin.git", subPath: "plugin", name: "demo"}
	localDir := t.TempDir()
	local := source{local: localDir, name: "demo"}
	if err := validateSourceBuildPolicy(remote, pluginGo); err != nil {
		t.Fatalf("remote Go source unexpectedly refused: %v", err)
	}
	if err := validateSourceBuildPolicy(local, pluginRust); err == nil || !strings.Contains(err.Error(), "Cargo.lock") {
		t.Fatalf("unlocked local Rust source err = %v", err)
	}
	writeTestFile(t, filepath.Join(localDir, "Cargo.lock"), "# reviewed lock\n")
	if err := validateSourceBuildPolicy(local, pluginRust); err != nil {
		t.Fatalf("reviewed local Rust source unexpectedly refused: %v", err)
	}
	if err := validateSourceBuildPolicy(remote, pluginRust); err == nil || !strings.Contains(err.Error(), "native Cargo build scripts") {
		t.Fatalf("remote Rust source err = %v", err)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
