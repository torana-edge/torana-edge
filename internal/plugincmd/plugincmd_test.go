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
	if !strings.Contains(string(goMod), "github.com/torana-edge/torana-plugin-sdk v0.1.0") {
		t.Fatalf("standalone SDK dependency missing:\n%s", goMod)
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
