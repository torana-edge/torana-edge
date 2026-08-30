package pluginfiles

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/torana-edge/torana-edge/internal/wasm"
)

func fileResource(max int64, retained int) wasm.FileResource {
	return wasm.FileResource{MaxBytes: max, RetainedFiles: retained, Operations: map[string]bool{"append": true, "read": true}}
}

func TestAppendRotatesAndIsolatesPlugins(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resource := fileResource(5, 2)
	if err := store.Append("a", "usage.jsonl", []byte("abc"), resource); err != nil {
		t.Fatal(err)
	}
	if err := store.Append("a", "usage.jsonl", []byte("def"), resource); err != nil {
		t.Fatal(err)
	}
	got, err := store.Read("a", "usage.jsonl", resource)
	if err != nil || string(got) != "def" {
		t.Fatalf("active = %q, %v", got, err)
	}
	rotated, err := store.OperatorRead("a", "usage.jsonl.1")
	if err != nil || string(rotated) != "abc" {
		t.Fatalf("rotated = %q, %v", rotated, err)
	}
	if _, err := store.OperatorRead("b", "usage.jsonl"); !os.IsNotExist(err) {
		t.Fatalf("plugin b read plugin a data: %v", err)
	}
	if err := store.Append("a", "usage.jsonl", bytes.Repeat([]byte("x"), 6), resource); err == nil {
		t.Fatal("single append larger than the approved file was accepted")
	}
}

func TestStoreRejectsTraversalAndLinks(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	resource := fileResource(1024, 1)
	for _, path := range []string{"../secret", "/absolute", "a//b", `a\\b`} {
		if err := store.Append("plugin", path, []byte("x"), resource); err == nil {
			t.Errorf("unsafe path %q accepted", path)
		}
	}
	base := pluginDir(root, "plugin")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := store.Append("plugin", "linked", []byte("x"), resource); err == nil {
		t.Fatal("symlink target accepted")
	}
}
