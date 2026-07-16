package cache

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// newTestRedis returns a RedisStore backed by an in-process miniredis.
func newTestRedis(t *testing.T, ttl time.Duration) (*RedisStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	store, err := NewRedisStore(mr.Addr(), "", 0, "torana:", ttl)
	if err != nil {
		t.Fatalf("NewRedisStore: %v", err)
	}
	t.Cleanup(store.Close)
	return store, mr
}

// TestStoreContract runs the same behavioral suite against both backends —
// anything a plugin can observe must be identical.
func TestStoreContract(t *testing.T) {
	backends := []struct {
		name string
		make func(t *testing.T) Store
	}{
		{"memory", func(t *testing.T) Store {
			s := NewLocalCache(time.Minute)
			t.Cleanup(s.Close)
			return s
		}},
		{"redis", func(t *testing.T) Store {
			s, _ := newTestRedis(t, time.Minute)
			return s
		}},
	}

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			s := b.make(t)

			if _, ok := s.Get("missing"); ok {
				t.Error("Get(missing) should miss")
			}
			s.Set("k1", "v1")
			s.Set("k2", "v2")
			if v, ok := s.Get("k1"); !ok || v != "v1" {
				t.Errorf("Get(k1) = %q,%v", v, ok)
			}
			if n := s.Len(); n != 2 {
				t.Errorf("Len = %d, want 2", n)
			}
			s.Set("k1", "v1b") // overwrite
			if v, _ := s.Get("k1"); v != "v1b" {
				t.Errorf("overwrite: got %q", v)
			}
			s.Delete("k2")
			if _, ok := s.Get("k2"); ok {
				t.Error("Get after Delete should miss")
			}

			// Concurrent access must be safe.
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					for j := 0; j < 50; j++ {
						key := fmt.Sprintf("c%d-%d", i, j)
						s.Set(key, "x")
						s.Get(key)
					}
				}(i)
			}
			wg.Wait()
		})
	}
}

// TestRedisTTLExpiry: entries expire after the configured TTL (miniredis
// clock is advanced manually).
func TestRedisTTLExpiry(t *testing.T) {
	store, mr := newTestRedis(t, 100*time.Millisecond)
	store.Set("k", "v")
	if _, ok := store.Get("k"); !ok {
		t.Fatal("entry should exist before TTL")
	}
	mr.FastForward(200 * time.Millisecond)
	if _, ok := store.Get("k"); ok {
		t.Fatal("entry should have expired")
	}
}

// TestRedisDownDegradesToMiss: a dead Redis degrades every op to a cache
// miss — it must never error a request.
func TestRedisDownDegradesToMiss(t *testing.T) {
	store, mr := newTestRedis(t, time.Minute)
	store.Set("k", "v")
	mr.Close()
	if _, ok := store.Get("k"); ok {
		t.Fatal("Get against a dead Redis should miss, not hang or panic")
	}
	store.Set("k2", "v2") // must not panic
	store.Delete("k")     // must not panic
}

// TestNewFromConfig: backend selection, defaults, and unknown-backend error.
func TestNewFromConfig(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	if _, ok := s.(*LocalCache); !ok {
		t.Fatalf("default backend = %T, want *LocalCache", s)
	}
	s.Close()

	if _, err := New(Config{Backend: "bogus"}); err == nil {
		t.Fatal("unknown backend should error")
	}

	mr := miniredis.RunT(t)
	s, err = New(Config{Backend: "redis", Redis: RedisConfig{Addr: mr.Addr()}})
	if err != nil {
		t.Fatalf("redis config: %v", err)
	}
	defer s.Close()
	s.Set("k", "v")
	if v, ok := s.Get("k"); !ok || v != "v" {
		t.Fatalf("redis store roundtrip failed: %q %v", v, ok)
	}

	// Unreachable redis must fail fast, not fall back silently.
	if _, err := New(Config{Backend: "redis", Redis: RedisConfig{Addr: "127.0.0.1:1"}}); err == nil {
		t.Fatal("unreachable redis should be a hard error")
	}
}
