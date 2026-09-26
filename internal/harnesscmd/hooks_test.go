package harnesscmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectHookCLIExplicitOptInPreviewInstallAndTeardown(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	args := []string{"harness", "hooks", "setup", "claude-code", "--scope", "project", "--addr", "http://127.0.0.1:8080"}
	var out, diag bytes.Buffer
	if err := Run(append(append([]string(nil), args...), "--dry-run"), strings.NewReader(""), &out, &diag); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, ".claude", "settings.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("preview wrote project settings")
	}
	if err := Run(append(append([]string(nil), args...), "--yes"), strings.NewReader(""), &out, &diag); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(raw), "PreModelSwitch") || !strings.Contains(string(raw), "PostModelSwitch") {
		t.Fatalf("settings=%s %v", raw, err)
	}
	args[2] = "teardown"
	if err := Run(append(args, "--yes"), strings.NewReader(""), &out, &diag); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	if err != nil || strings.Contains(string(raw), "hooks") {
		t.Fatalf("teardown=%s %v", raw, err)
	}
}
