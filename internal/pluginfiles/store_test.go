package pluginfiles

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/torana-edge/torana-edge/internal/wasm"
)

func fileResource(maxBytes int64, retained int) wasm.FileResource {
	return wasm.FileResource{MaxBytes: maxBytes, RetainedFiles: retained, Operations: map[string]bool{"append": true, "read": true}}
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
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	var appends sync.WaitGroup
	t.Cleanup(func() {
		releaseAll()
		appends.Wait()
	})
	store.syncFile = func(file *os.File) error {
		entered <- file.Name()
		<-release
		return nil
	}
	resource := fileResource(1024, 1)
	errCh := make(chan error, 4)
	appendAsync := func(plugin, logical, data string) {
		appends.Add(1)
		go func() {
			defer appends.Done()
			errCh <- store.Append(plugin, logical, []byte(data), resource)
		}()
	}
	nextSync := func() string {
		t.Helper()
		select {
		case name := <-entered:
			return name
		case err := <-errCh:
			t.Fatalf("append failed before storage sync: %v", err)
			return ""
		}
	}
	appendAsync("plugin-a", "one.log", "a")
	first := nextSync()
	fileLock := store.pluginLocks("plugin-a").file(first)
	if !fileLock.TryLock() {
		t.Fatal("append retained its mutation lock during storage sync")
	}
	fileLock.Unlock()
	appendAsync("plugin-a", "one.log", "b")
	appendAsync("plugin-a", "two.log", "c")
	appendAsync("plugin-b", "two.log", "d")

	sameFileSyncs := 0
	for range 3 {
		if concurrent := nextSync(); concurrent == first {
			sameFileSyncs++
		}
	}
	if sameFileSyncs != 1 {
		t.Fatalf("same-file concurrent syncs = %d, want one follow-up append", sameFileSyncs)
	}
	releaseAll()
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

func TestOperatorPathIsAbsoluteSafeAndDoesNotRequireExistingFile(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.OperatorPath("usage_logger", "nested/usage.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("path is not absolute: %q", path)
	}
	want := filepath.Join(pluginDir(root, "usage_logger"), "nested", "usage.jsonl")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("path lookup created the file: %v", err)
	}
	for _, logical := range []string{"", "../usage.jsonl", "/tmp/usage.jsonl", `nested\usage.jsonl`} {
		if got, err := store.OperatorPath("usage_logger", logical); err == nil {
			t.Errorf("unsafe path %q resolved to %q", logical, got)
		}
	}
	if got, err := store.OperatorPath("", "usage.jsonl"); err == nil {
		t.Errorf("empty plugin resolved to %q", got)
	}
}

// The hard-link guard is a security boundary: a plugin-private file with more
// than one directory entry is a link the operator did not create, and writing
// through it escapes the plugin's own directory.
//
// This is deliberately platform-agnostic. The guard used to read
// syscall.Stat_t.Nlink directly, which does not exist on Windows — and the
// first attempt at portability returned "link count unknown" there, which would
// have silently dropped the boundary on a released platform while Unix kept
// enforcing it. Both halves are pinned here so any platform that compiles must
// also behave.
func TestRegularSingleLinkRefusesMultipleLinks(t *testing.T) {
	dir := t.TempDir()
	single := filepath.Join(dir, "single.jsonl")
	if err := os.WriteFile(single, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// One link: accepted.
	if err := regularSingleLink(single, false); err != nil {
		t.Fatalf("a file with exactly one link must be accepted: %v", err)
	}
	if n, err := hardLinkCount(single, mustStat(t, single)); err != nil || n != 1 {
		t.Fatalf("hardLinkCount = %d, %v; want 1, nil", n, err)
	}

	// A second link: refused.
	linked := filepath.Join(dir, "linked.jsonl")
	if err := os.Link(single, linked); err != nil {
		t.Skipf("this filesystem does not support hard links: %v", err)
	}
	if err := regularSingleLink(single, false); err == nil {
		t.Error("a file with two directory entries must be refused; writing through it escapes the plugin directory")
	}
	if n, err := hardLinkCount(single, mustStat(t, single)); err != nil || n != 2 {
		t.Errorf("hardLinkCount = %d, %v; want 2, nil", n, err)
	}
}

// A count that cannot be obtained must be an error, never a pass.
func TestRegularSingleLinkFailsClosedOnMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.jsonl")
	if err := regularSingleLink(missing, true); err != nil {
		t.Errorf("allowMissing must tolerate absence: %v", err)
	}
	if err := regularSingleLink(missing, false); err == nil {
		t.Error("a missing file must be refused when it is required to exist")
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return info
}
