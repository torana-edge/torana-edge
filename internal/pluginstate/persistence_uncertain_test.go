package pluginstate

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// bbolt owns the durable transaction boundary now: readers see either the old
// page generation or the committed new one, and concurrent writers serialize.
func TestPersistentConcurrentReadersAndWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := newStore(t, Options{Path: path})
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Set("p", "k", "seed"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, ok, err := s.Get("p", "k"); err != nil || !ok {
				t.Error("reader observed a missing committed key")
			}
		}()
		go func(i int) {
			defer wg.Done()
			if err := s.Set("p", "k", string(rune('a'+i%26))); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if _, version, ok, err := s.GetVersioned("p", "k"); err != nil || !ok || version == "" {
		t.Fatal("final committed value/version missing")
	}
}

func TestOpenRejectsInconsistentAccounting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := newStore(t, Options{Path: path})
	if err := store.Set("p", "k", "value"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return putUint64(tx.Bucket(bucketMetadata), keyTotalBytes, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Path: path}); err == nil || !strings.Contains(err.Error(), "accounting mismatch") {
		t.Fatalf("inconsistent database accepted: %v", err)
	}
}

func TestCloseAndReopenReleasesDatabaseLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first := newStore(t, Options{Path: path})
	if err := first.Set("p", "k", "value"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := newStore(t, Options{Path: path})
	t.Cleanup(func() { _ = second.Close() })
	if value, ok, err := second.Get("p", "k"); err != nil || !ok || value != "value" {
		t.Fatalf("reopened value = %q, %v", value, ok)
	}
}
