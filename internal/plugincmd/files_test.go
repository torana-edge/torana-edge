package plugincmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/pluginfiles"
)

func TestPluginFilePathPrintsOnlyAbsoluteManagedPath(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("TORANA_DATA_DIR", dataDir)

	var stdout bytes.Buffer
	if err := pluginFile([]string{"path", "usage_logger", "usage.jsonl"}, &stdout); err != nil {
		t.Fatalf("pluginFile: %v", err)
	}
	store, err := pluginfiles.New(filepath.Join(dataDir, "plugin-data"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := store.OperatorPath("usage_logger", "usage.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != want+"\n" {
		t.Fatalf("stdout = %q, want only %q", got, want+"\n")
	}
	if !filepath.IsAbs(strings.TrimSpace(stdout.String())) {
		t.Fatalf("path is not absolute: %q", stdout.String())
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("path lookup created the logical file: %v", err)
	}
}

func TestPluginFilePathRejectsUnsafeAndMalformedInputs(t *testing.T) {
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	for _, args := range [][]string{
		{"path", "usage_logger"},
		{"path", "usage_logger", "usage.jsonl", "extra"},
		{"path", "", "usage.jsonl"},
		{"path", "usage_logger", ""},
		{"path", "usage_logger", "../usage.jsonl"},
		{"path", "usage_logger", "/tmp/usage.jsonl"},
		{"path", "usage_logger", `nested\\usage.jsonl`},
	} {
		var stdout bytes.Buffer
		if err := pluginFile(args, &stdout); err == nil {
			t.Errorf("pluginFile(%q) succeeded with stdout %q", args, stdout.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("pluginFile(%q) wrote stdout on failure: %q", args, stdout.String())
		}
	}
}
