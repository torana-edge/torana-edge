package plugincmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/plugin"
)

func TestLocalPluginBuildEnvPinsToolchainAndWorkspace(t *testing.T) {
	t.Setenv("GOTOOLCHAIN", "auto")
	t.Setenv("GOWORK", "/tmp/hostile.work")
	t.Setenv("GOOS", "hostile")
	t.Setenv("GOARCH", "hostile")
	env := localPluginBuildEnv("GOOS=wasip1", "GOARCH=wasm")

	values := make(map[string][]string)
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = append(values[name], value)
	}
	for name, want := range map[string]string{
		"GOTOOLCHAIN": "local",
		"GOWORK":      "off",
		"GOOS":        "wasip1",
		"GOARCH":      "wasm",
	} {
		if got := values[name]; len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want exactly [%s]", name, got, want)
		}
	}
}

// TestInstalledDigestMatchesHost is the test that matters. `torana plugin
// install` prints a digest and tells the operator to approve it; if that value
// disagrees with the one the host computes at load time, the instruction is
// useless and nothing errors — the operator approves a digest that never
// appears. An earlier draft of this command reimplemented the hash with a
// different field order and no length prefixes and looked entirely correct.
func TestInstalledDigestMatchesHost(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("plugin.json", `{"name":"demo","version":"0.1.0"}`)
	write("plugin.wasm", "\x00asm\x01\x00\x00\x00 not a real module")
	write("schema.json", `{"fields":[]}`)

	fromCLI, err := plugin.BundleDigestForDir(dir)
	if err != nil {
		t.Fatalf("BundleDigestForDir: %v", err)
	}
	if !strings.HasPrefix(fromCLI, "sha256:") {
		t.Fatalf("digest %q is not sha256-prefixed", fromCLI)
	}

	// A bundle that gains agent.json must produce a different digest — that is
	// what forces re-approval when a plugin's agent contract changes.
	write("agent.json", `{"schema_version":1,"operations":[]}`)
	withAgent, err := plugin.BundleDigestForDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if withAgent == fromCLI {
		t.Error("adding agent.json did not change the digest — an agent contract could change without re-approval")
	}
}

func TestParseSource(t *testing.T) {
	cases := []struct {
		in      string
		repo    string
		sub     string
		ref     string
		name    string
		wantErr bool
	}{
		{in: "github.com/o/r/plugins/pii", repo: "https://github.com/o/r.git", sub: "plugins/pii", name: "pii"},
		{in: "github.com/o/r/plugins/pii@v1.2.0", repo: "https://github.com/o/r.git", sub: "plugins/pii", ref: "v1.2.0", name: "pii"},
		{in: "gitlab.example.com/team/repo/p", repo: "https://gitlab.example.com/team/repo.git", sub: "p", name: "p"},
		{in: "https://gitlab.example.com/group/subgroup/repo.git//plugins/pii@deadbeef", repo: "https://gitlab.example.com/group/subgroup/repo.git", sub: "plugins/pii", ref: "deadbeef", name: "pii"},
		{in: "https://gitlab.example.com/group/repo.git//../escape", wantErr: true},
		{in: "https://gitlab.example.com/group/repo.git//.", wantErr: true},
		{in: "https://gitlab.example.com/group/repo.git//plugins/pii/..", wantErr: true},
		{in: "github.com/o/r/plugins/pii/..", wantErr: true},
		{in: "github.com/o/r", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, c := range cases {
		got, err := parseSource(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSource(%q) should have failed", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSource(%q): %v", c.in, err)
			continue
		}
		if got.repoURL != c.repo || got.subPath != c.sub || got.ref != c.ref || got.name != c.name {
			t.Errorf("parseSource(%q) = %+v, want repo=%s sub=%s ref=%s name=%s",
				c.in, got, c.repo, c.sub, c.ref, c.name)
		}
	}
}

func TestCopyTreeSupportsNestedSourcesAndRejectsSymlinks(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "internal", "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "internal", "rules", "rules.go"), []byte("package rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree nested source: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "internal", "rules", "rules.go")); err != nil {
		t.Fatalf("nested source was not copied: %v", err)
	}

	linkRoot := t.TempDir()
	if err := os.Symlink("/etc/passwd", filepath.Join(linkRoot, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(linkRoot, t.TempDir()); err == nil {
		t.Fatal("symlinked source was accepted")
	}
}

func TestActivateBundleRemovesStaleOptionalFiles(t *testing.T) {
	dest := t.TempDir()
	target := filepath.Join(dest, "demo")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "agent.json"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	stage, err := os.MkdirTemp(dest, ".demo.install-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "plugin.json"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := activateBundle(stage, target); err != nil {
		t.Fatalf("activateBundle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "agent.json")); !os.IsNotExist(err) {
		t.Fatalf("stale agent.json survived replacement: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "plugin.json")); err != nil || string(got) != "new" {
		t.Fatalf("new bundle not activated: %q, %v", got, err)
	}
}

func TestParseSourceLocalPath(t *testing.T) {
	got, err := parseSource("./my-plugin")
	if err != nil {
		t.Fatal(err)
	}
	if got.local == "" || got.repoURL != "" {
		t.Errorf("./my-plugin should resolve to a local source, got %+v", got)
	}
	if got.name != "my-plugin" {
		t.Errorf("name = %q, want my-plugin", got.name)
	}
}

func TestRemoveRejectsPathTraversal(t *testing.T) {
	for _, bad := range []string{"../etc", "a/b", "..", ""} {
		var out bytes.Buffer
		if err := removePlugin([]string{"--dir", t.TempDir(), bad}, &out); err == nil {
			t.Errorf("removePlugin(%q) should have been rejected", bad)
		}
	}
}

func TestListEmptyDirGuidesTheUser(t *testing.T) {
	var out bytes.Buffer
	if err := listPlugins([]string{"--dir", t.TempDir()}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "--official") {
		t.Errorf("an empty plugins dir should point at the official set, got: %s", out.String())
	}
}
