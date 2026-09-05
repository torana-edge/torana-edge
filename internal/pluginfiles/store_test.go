package pluginfiles

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/wasm"
)

func fileResource(max int64, retained int) wasm.FileResource {
	return wasm.FileResource{MaxBytes: max, RetainedFiles: retained, Operations: map[string]bool{"append": true, "read": true}}
}

func TestRetainedGenerationsShareTheirMutationLock(t *testing.T) {
	locks := &pluginLocks{}
	base := filepath.Join(t.TempDir(), "usage.jsonl")
	if locks.file(base) != locks.file(base+".1") || locks.file(base) != locks.file(base+".20") ||
		locks.file(base) != locks.file(base+".1.2") {
		t.Fatal("retained generations do not share the base-file lock")
	}
	if locks.file(base) == locks.file(filepath.Join(filepath.Dir(base), "other.jsonl")) {
		t.Fatal("independent files unexpectedly share a lock")
	}
}

func TestAppendOrdersWritesWithoutSerializingStorageFlushes(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan string, 4)
	release := make(chan struct{})
	store.syncFile = func(file *os.File) error {
		entered <- file.Name()
		<-release
		return nil
	}
	resource := fileResource(1024, 1)
	errCh := make(chan error, 4)
	go func() { errCh <- store.Append("plugin-a", "one.log", []byte("a"), resource) }()
	first := <-entered
	go func() { errCh <- store.Append("plugin-a", "one.log", []byte("b"), resource) }()
	go func() { errCh <- store.Append("plugin-a", "two.log", []byte("c"), resource) }()
	go func() { errCh <- store.Append("plugin-b", "two.log", []byte("d"), resource) }()

	sameFileSyncs := 0
	for range 3 {
		select {
		case concurrent := <-entered:
			if concurrent == first {
				sameFileSyncs++
			}
		case <-time.After(time.Second):
			t.Fatal("an append was blocked behind another append's storage sync")
		}
	}
	if sameFileSyncs != 1 {
		t.Fatalf("same-file concurrent syncs = %d, want one follow-up append", sameFileSyncs)
	}
	close(release)
	for range 4 {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	data, err := store.Read("plugin-a", "one.log", resource)
	if err != nil || string(data) != "ab" {
		t.Fatalf("same-file append order = %q, %v; want ab", data, err)
	}
}

func TestDeleteRemovesEveryRotationOnly(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resource := fileResource(1, 2)
	for _, value := range []string{"a", "b", "c"} {
		if err := store.Append("plugin", "usage.jsonl", []byte(value), resource); err != nil {
			t.Fatal(err)
		}
	}
	base := pluginDir(store.root, "plugin")
	if err := os.WriteFile(filepath.Join(base, "usage.jsonl.notes"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("plugin", "usage.jsonl", resource); err != nil {
		t.Fatal(err)
	}
	for _, logical := range []string{"usage.jsonl", "usage.jsonl.1", "usage.jsonl.2"} {
		if _, err := store.OperatorRead("plugin", logical); !os.IsNotExist(err) {
			t.Fatalf("%s remains readable after delete: %v", logical, err)
		}
	}
	data, err := store.OperatorRead("plugin", "usage.jsonl.notes")
	if err != nil || string(data) != "keep" {
		t.Fatalf("non-generation sibling = %q, %v", data, err)
	}
}

func TestDeleteValidatesEveryGenerationBeforeRemovingAny(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resource := fileResource(1024, 2)
	if err := store.Append("plugin", "usage.jsonl", []byte("current"), resource); err != nil {
		t.Fatal(err)
	}
	base := pluginDir(store.root, "plugin")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "usage.jsonl.1")); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("plugin", "usage.jsonl", resource); err == nil {
		t.Fatal("delete accepted a linked retained generation")
	}
	data, err := store.OperatorRead("plugin", "usage.jsonl")
	if err != nil || string(data) != "current" {
		t.Fatalf("current generation changed after refused delete: %q, %v", data, err)
	}
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
