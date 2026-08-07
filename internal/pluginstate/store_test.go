package pluginstate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newStore(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestSetGetRoundTrip(t *testing.T) {
	s := newStore(t, Options{})
	if err := s.Set("warmer", "conv-a3f9", `{"deadline":123}`); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok := s.Get("warmer", "conv-a3f9")
	if !ok || got != `{"deadline":123}` {
		t.Errorf("Get = %q (ok=%v), want the stored value", got, ok)
	}
}

// TestNamespacedPerPlugin is the security-relevant property: durable state is
// private, like env.cache_* and unlike the explicit env.shared_cache_* channel.
// A plugin persisting something sensitive must not hand it to its neighbours.
func TestNamespacedPerPlugin(t *testing.T) {
	s := newStore(t, Options{})
	if err := s.Set("warmer", "shared-key", "warmer's value"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("evil", "shared-key", "evil's value"); err != nil {
		t.Fatal(err)
	}

	if got, _ := s.Get("warmer", "shared-key"); got != "warmer's value" {
		t.Errorf("warmer read %q — namespaces merged", got)
	}
	if got, _ := s.Get("evil", "shared-key"); got != "evil's value" {
		t.Errorf("evil read %q — namespaces merged", got)
	}
}

// TestSurvivesRestart is the whole reason this package exists. env.cache_* is
// lost on restart; a warming plugin that forgot its prefixes would silently
// stop working after every deploy.
func TestSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	first := newStore(t, Options{Path: path})
	if err := first.Set("warmer", "conv-a3f9", "prefix-bytes"); err != nil {
		t.Fatal(err)
	}

	second := newStore(t, Options{Path: path})
	got, ok := second.Get("warmer", "conv-a3f9")
	if !ok || got != "prefix-bytes" {
		t.Errorf("after restart Get = %q (ok=%v), want the persisted value", got, ok)
	}
	if second.TotalBytes() != first.TotalBytes() {
		t.Errorf("byte accounting did not survive reload: %d vs %d", second.TotalBytes(), first.TotalBytes())
	}
}

// TestEmptyValueDeletes — a plugin releases state by writing "", so it needs no
// separate delete host call.
// An empty value is stored; Delete removes. This test asserted the opposite,
// which was the v1 rule — and under it a plugin could neither store an empty
// string nor distinguish absence from emptiness.
func TestEmptyValueIsStoredAndDeleteRemoves(t *testing.T) {
	s := newStore(t, Options{})
	if err := s.Set("warmer", "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("warmer", "k", ""); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("warmer", "k")
	if !ok {
		t.Fatal("writing an empty value deleted the key")
	}
	if got != "" {
		t.Fatalf("got %q, want an empty value", got)
	}

	if err := s.Delete("warmer", "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("warmer", "k"); ok {
		t.Error("Delete did not remove the key")
	}
	if s.TotalBytes() != 0 {
		t.Errorf("TotalBytes = %d after delete, want 0", s.TotalBytes())
	}
}

// Deleting a key that was never written succeeds: the caller wants it gone.
func TestDeleteIsIdempotent(t *testing.T) {
	s := newStore(t, Options{})
	if err := s.Delete("warmer", "never-existed"); err != nil {
		t.Fatalf("deleting an absent key failed: %v", err)
	}
}

func TestValueSizeLimit(t *testing.T) {
	s := newStore(t, Options{MaxValueBytes: 100})
	err := s.Set("warmer", "k", strings.Repeat("x", 101))
	if err == nil {
		t.Fatal("an oversized value was accepted")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error does not name the limit: %v", err)
	}
}

// TestTotalSizeLimit — a buggy plugin must not be able to fill the disk.
func TestTotalSizeLimit(t *testing.T) {
	s := newStore(t, Options{MaxTotalBytes: 200, MaxValueBytes: 100})
	for i := 0; i < 10; i++ {
		if err := s.Set("warmer", fmt.Sprintf("key-%d", i), strings.Repeat("x", 50)); err != nil {
			if !strings.Contains(err.Error(), "limit") {
				t.Fatalf("unexpected error: %v", err)
			}
			if s.TotalBytes() > 200 {
				t.Errorf("TotalBytes = %d, exceeded the cap before refusing", s.TotalBytes())
			}
			return
		}
	}
	t.Error("the total-size cap was never enforced")
}

func TestPerPluginKeyLimit(t *testing.T) {
	s := newStore(t, Options{MaxKeysPerPlugin: 3})
	for i := 0; i < 3; i++ {
		if err := s.Set("warmer", fmt.Sprintf("k%d", i), "v"); err != nil {
			t.Fatalf("Set %d: %v", i, err)
		}
	}
	if err := s.Set("warmer", "k3", "v"); err == nil {
		t.Fatal("the key-count cap was not enforced")
	}
	// Overwriting an existing key must still work at the cap.
	if err := s.Set("warmer", "k0", "updated"); err != nil {
		t.Errorf("overwrite at the cap was refused: %v", err)
	}
}

// TestKeysAreSorted — plugins iterate keys they did not choose, and an order
// that changes between calls makes their behaviour irreproducible.
func TestKeysAreSorted(t *testing.T) {
	s := newStore(t, Options{})
	for _, k := range []string{"zeta", "alpha", "mu"} {
		if err := s.Set("warmer", k, "v"); err != nil {
			t.Fatal(err)
		}
	}
	got := s.Keys("warmer")
	want := []string{"alpha", "mu", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Keys = %v, want %v", got, want)
		}
	}
	if s.Keys("nobody") != nil {
		t.Error("Keys returned data for an unknown plugin")
	}
}

// TestCorruptFileDoesNotBlockStartup — plugin state is a convenience, and
// refusing to boot the proxy because one plugin's scratch file was truncated
// would be a bad trade.
func TestCorruptFileDoesNotBlockStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Path: path})
	if err == nil {
		t.Error("a corrupt file should be reported")
	}
	if s == nil {
		t.Fatal("a corrupt file must still yield a usable store")
	}
	if err := s.Set("warmer", "k", "v"); err != nil {
		t.Errorf("store unusable after a corrupt load: %v", err)
	}
}

// TestFilePermissions — state can hold prompt fragments, so it must not be
// world-readable.
func TestFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := newStore(t, Options{Path: path})
	if err := s.Set("warmer", "k", "v"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file mode = %o, want 600", perm)
	}
}

// TestNoPathIsMemoryOnly — a proxy without a data directory must still run.
func TestNoPathIsMemoryOnly(t *testing.T) {
	s := newStore(t, Options{})
	if err := s.Set("warmer", "k", "v"); err != nil {
		t.Fatalf("memory-only Set failed: %v", err)
	}
	if got, ok := s.Get("warmer", "k"); !ok || got != "v" {
		t.Error("memory-only store did not retain the value")
	}
}

func TestNilStoreIsInert(t *testing.T) {
	var s *Store
	if _, ok := s.Get("p", "k"); ok {
		t.Error("nil store returned a value")
	}
	if err := s.Set("p", "k", "v"); err == nil {
		t.Error("nil store accepted a write")
	}
	if s.Keys("p") != nil || s.Len("p") != 0 || s.TotalBytes() != 0 {
		t.Error("nil store reported data")
	}
}

func TestRejectsEmptyPluginOrKey(t *testing.T) {
	s := newStore(t, Options{})
	if err := s.Set("", "k", "v"); err == nil {
		t.Error("an empty plugin name was accepted")
	}
	if err := s.Set("p", "", "v"); err == nil {
		t.Error("an empty key was accepted")
	}
}

// TestConcurrentAccess runs under -race. Ticks and requests both write state,
// so concurrency is the normal case rather than an edge one.
func TestConcurrentAccess(t *testing.T) {
	s := newStore(t, Options{Path: filepath.Join(t.TempDir(), "state.json")})

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			plugin := fmt.Sprintf("plugin-%d", i%4)
			if err := s.Set(plugin, fmt.Sprintf("key-%d", i), "value"); err != nil {
				t.Errorf("Set: %v", err)
			}
			_, _ = s.Get(plugin, "key-0")
			_ = s.Keys(plugin)
		}(i)
	}
	wg.Wait()

	total := 0
	for i := 0; i < 4; i++ {
		total += s.Len(fmt.Sprintf("plugin-%d", i))
	}
	if total != 40 {
		t.Errorf("stored %d keys, want 40 — a write was lost", total)
	}
	reloaded := newStore(t, Options{Path: s.path})
	reloadedTotal := 0
	for i := 0; i < 4; i++ {
		reloadedTotal += reloaded.Len(fmt.Sprintf("plugin-%d", i))
	}
	if reloadedTotal != 40 {
		t.Errorf("persisted %d keys after concurrent writes, want 40", reloadedTotal)
	}
}

func TestFailedFlushRemainsDirtyAndCanBeRetried(t *testing.T) {
	root := t.TempDir()
	blockedPath := filepath.Join(root, "existing-directory")
	if err := os.Mkdir(blockedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	s := newStore(t, Options{Path: ""})
	s.path = blockedPath
	if err := s.Set("plugin", "key", "newest"); err == nil {
		t.Fatal("flush over an existing directory unexpectedly succeeded")
	}
	if !s.dirty {
		t.Fatal("failed flush cleared dirty state")
	}

	s.path = filepath.Join(root, "state.json")
	if err := s.flush(); err != nil {
		t.Fatalf("retry flush: %v", err)
	}
	reloaded := newStore(t, Options{Path: s.path})
	if got, ok := reloaded.Get("plugin", "key"); !ok || got != "newest" {
		t.Fatalf("retried snapshot = %q (ok=%v), want newest", got, ok)
	}
}
