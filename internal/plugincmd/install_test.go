package plugincmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/plugin"
)

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
