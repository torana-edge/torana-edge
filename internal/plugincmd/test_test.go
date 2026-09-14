package plugincmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginTestRequiresCompiledBundle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"schema_version":1,"id":"test/x","name":"x","version":"0.1.0","abi_version":"v1","failure_mode":"pass","hooks":[],"permissions":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scenario.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	err := Run([]string{"plugin", "test", dir}, &out, &stderr)
	if err == nil || !strings.Contains(err.Error(), "plugin.wasm") {
		t.Fatalf("err=%v", err)
	}
}

func TestPluginTestRejectsMalformedScenario(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{"request":`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	err := Run([]string{"plugin", "test", dir, "--scenario", path}, &out, &stderr)
	if err == nil || !strings.Contains(err.Error(), "parse scenario") {
		t.Fatalf("err=%v", err)
	}
}
