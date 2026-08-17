package plugincmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type pluginLanguage string

const (
	pluginGo   pluginLanguage = "Go"
	pluginRust pluginLanguage = "Rust"
)

// detectPluginLanguage makes the source/toolchain decision explicit. Guessing
// from source extensions is ambiguous for repositories that include examples
// or generators, and silently preferring one manifest makes the reviewed source
// disagree with what the operator actually built.
func detectPluginLanguage(dir string) (pluginLanguage, error) {
	hasGo, err := regularFileExists(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", err
	}
	hasRust, err := regularFileExists(filepath.Join(dir, "Cargo.toml"))
	if err != nil {
		return "", err
	}
	switch {
	case hasGo && hasRust:
		return "", errors.New("plugin source contains both go.mod and Cargo.toml; keep exactly one plugin build manifest")
	case hasGo:
		return pluginGo, nil
	case hasRust:
		return pluginRust, nil
	default:
		return "", errors.New("plugin source needs exactly one go.mod or Cargo.toml")
	}
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", path)
	}
	return true, nil
}

// buildPluginSource compiles one already-staged source tree. Callers own the
// staging boundary so neither Go's go.sum nor Cargo's lock/target output can
// mutate the source the operator is reviewing.
func buildPluginSource(dir, out string, rustLocked bool, stdout, stderr io.Writer) (pluginLanguage, error) {
	language, err := detectPluginLanguage(dir)
	if err != nil {
		return "", err
	}
	switch language {
	case pluginGo:
		if err := buildGoPlugin(dir, out, stdout, stderr); err != nil {
			return "", err
		}
	case pluginRust:
		if err := buildRustPlugin(dir, out, rustLocked, stdout, stderr); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unsupported plugin language %q", language)
	}
	return language, nil
}

func buildGoPlugin(dir, out string, stdout, stderr io.Writer) error {
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	tidy.Env = localPluginBuildEnv()
	tidy.Stdout = stdout
	tidy.Stderr = stderr
	if err := tidy.Run(); err != nil {
		return fmt.Errorf("resolve Go plugin dependencies: %w", err)
	}
	cmd := exec.Command("go", "build", "-trimpath", "-buildmode=c-shared", "-buildvcs=false", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = localPluginBuildEnv("GOOS=wasip1", "GOARCH=wasm")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build Go plugin: %w", err)
	}
	return nil
}

func buildRustPlugin(dir, out string, locked bool, stdout, stderr io.Writer) error {
	if _, err := exec.LookPath("cargo"); err != nil {
		return errors.New("build Rust plugin: cargo is not installed (install Rust 1.85+ and the wasm32-wasip1 target)")
	}
	targetDir := filepath.Join(dir, ".torana-cargo-target")
	env, err := rustBuildEnv(targetDir)
	if err != nil {
		return err
	}
	args := []string{"build", "--release", "--target", "wasm32-wasip1"}
	if locked {
		args = append(args, "--locked")
	}
	cmd := exec.Command("cargo", args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build Rust plugin: %w (requires Rust 1.85+ and `rustup target add wasm32-wasip1`)", err)
	}

	releaseDir := filepath.Join(targetDir, "wasm32-wasip1", "release")
	entries, err := os.ReadDir(releaseDir)
	if err != nil {
		return fmt.Errorf("find Rust WASM artifact: %w", err)
	}
	var artifacts []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".wasm") {
			artifacts = append(artifacts, filepath.Join(releaseDir, entry.Name()))
		}
	}
	sort.Strings(artifacts)
	if len(artifacts) != 1 {
		return fmt.Errorf("Rust plugin build produced %d top-level WASM artifacts, want exactly one: %v", len(artifacts), artifacts)
	}
	data, err := os.ReadFile(artifacts[0])
	if err != nil {
		return fmt.Errorf("read Rust WASM artifact: %w", err)
	}
	if len(data) < 8 || string(data[:4]) != "\x00asm" {
		return errors.New("Rust plugin artifact is not a WebAssembly module")
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return fmt.Errorf("write Rust WASM artifact: %w", err)
	}
	return nil
}

// rustBuildEnv prevents a source-tree rust-toolchain file from selecting or
// downloading a different compiler before the resulting WASM can be reviewed.
// Cargo build scripts still execute native code, which is why remote Rust
// sources are refused by installPlugin and must first be cloned and reviewed.
func rustBuildEnv(targetDir string) ([]string, error) {
	env := replaceEnv(os.Environ(), "CARGO_TARGET_DIR", targetDir)
	if _, err := exec.LookPath("rustup"); err != nil {
		return env, nil // A system Cargo has no rustup source override to neutralize.
	}
	cmd := exec.Command("rustup", "show", "active-toolchain")
	cmd.Dir = string(filepath.Separator) // never inspect the plugin's directory override
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("resolve active Rust toolchain: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return nil, errors.New("resolve active Rust toolchain: rustup returned no toolchain")
	}
	return replaceEnv(env, "RUSTUP_TOOLCHAIN", fields[0]), nil
}

func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}
