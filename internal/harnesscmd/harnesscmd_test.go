package harnesscmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPaths(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for _, tc := range []struct{ name, scope, want string }{
		{"claude-code", "project", "/work/.mcp.json"},
		{"claude-code", "user", "/user/.claude.json"},
		{"codex", "project", "/work/.codex/config.toml"},
		{"codex", "user", "/custom/config.toml"},
	} {
		got, err := configPath(tc.name, tc.scope, "/work", "/user", "/custom")
		if err != nil || got != tc.want {
			t.Fatalf("%+v: %q %v", tc, got, err)
		}
	}
}

func TestProjectPreviewDeclineApplyAndTeardown(t *testing.T) {
	// Run is intentionally offline: setup does not enable MCP or grant trust.
	t.Chdir(t.TempDir())
	for _, name := range []string{"claude-code", "codex"} {
		var out bytes.Buffer
		invoke := func(command string, flags ...string) {
			t.Helper()
			out.Reset()
			args := append([]string{"harness", command, name}, flags...)
			if err := Run(args, strings.NewReader("n\n"), &out, &out); err != nil {
				t.Fatal(err)
			}
		}
		cwd, _ := os.Getwd()
		path, err := configPath(name, "project", cwd, "unused", "")
		if err != nil {
			t.Fatal(err)
		}
		invoke("setup", "--dry-run")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("preview wrote configuration")
		}
		invoke("setup")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("declined setup wrote configuration")
		}
		invoke("setup", "--yes")
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Contains(data, []byte("torana")) {
			t.Fatalf("setup: %s %v", data, err)
		}
		invoke("setup", "--yes")
		if !strings.Contains(out.String(), "No change needed") {
			t.Fatal(out.String())
		}
		invoke("teardown", "--yes")
		data, err = os.ReadFile(path)
		if err != nil || bytes.Contains(data, []byte("mcp_servers.torana")) || bytes.Contains(data, []byte(`"torana"`)) {
			t.Fatalf("teardown: %s %v", data, err)
		}
		backups, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".torana-backup-*"))
		if len(backups) == 0 {
			t.Fatal("teardown lacked recovery backup")
		}
	}
}

func TestRejectUnsupportedAndRemote(t *testing.T) {
	for _, args := range [][]string{
		{"setup", "unknown"}, {"setup", "codex", "--scope", "invalid"},
		{"setup", "codex", "--addr", "https://example.com"},
		{"setup", "codex", "unexpected"},
	} {
		var out bytes.Buffer
		if Run(args, strings.NewReader(""), &out, &out) == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}
