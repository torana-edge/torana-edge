package cache

import (
	"context"
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

			if _, ok := s.Get(context.Background(), "missing"); ok {
				t.Error("Get(missing) should miss")
			}
			s.Set(context.Background(), "k1", "v1")
			s.Set(context.Background(), "k2", "v2")
			if v, ok := s.Get(context.Background(), "k1"); !ok || v != "v1" {
				t.Errorf("Get(k1) = %q,%v", v, ok)
			}
			if n := s.Len(); n != 2 {
				t.Errorf("Len = %d, want 2", n)
			}
			s.Set(context.Background(), "k1", "v1b") // overwrite
			if v, _ := s.Get(context.Background(), "k1"); v != "v1b" {
				t.Errorf("overwrite: got %q", v)
			}
			s.Delete(context.Background(), "k2")
			if _, ok := s.Get(context.Background(), "k2"); ok {
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
						s.Set(context.Background(), key, "x")
						s.Get(context.Background(), key)
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
	store.Set(context.Background(), "k", "v")
	if _, ok := store.Get(context.Background(), "k"); !ok {
		t.Fatal("entry should exist before TTL")
	}
	mr.FastForward(200 * time.Millisecond)
	if _, ok := store.Get(context.Background(), "k"); ok {
		t.Fatal("entry should have expired")
	}
}

// TestRedisDownDegradesToMiss: a dead Redis degrades every op to a cache
// miss — it must never error a request.
func TestRedisDownDegradesToMiss(t *testing.T) {
	store, mr := newTestRedis(t, time.Minute)
	store.Set(context.Background(), "k", "v")
	mr.Close()
	if _, ok := store.Get(context.Background(), "k"); ok {
		t.Fatal("Get against a dead Redis should miss, not hang or panic")
	}
	store.Set(context.Background(), "k2", "v2") // must not panic
	store.Delete(context.Background(), "k")     // must not panic
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
	s.Set(context.Background(), "k", "v")
	if v, ok := s.Get(context.Background(), "k"); !ok || v != "v" {
		t.Fatalf("redis store roundtrip failed: %q %v", v, ok)
	}

	// Unreachable redis must fail fast, not fall back silently.
	if _, err := New(Config{Backend: "redis", Redis: RedisConfig{Addr: "127.0.0.1:1"}}); err == nil {
		t.Fatal("unreachable redis should be a hard error")
	}
}

// A cache call made for an abandoned request must stop when that request does.
//
// Every operation used to build its context from context.Background(), so the
// caller's cancellation reached nothing: a client that hung up still had its
// cache lookups run to completion, and a slow server added its full timeout to
// a live request. The comment on redisOpTimeout claimed the opposite — that it
// bounded "a cache call, never a request".
func TestRedisHonoursTheCallersCancellation(t *testing.T) {
	store, _ := newTestRedis(t, time.Minute)
	defer store.Close()

	store.Set(context.Background(), "k", "v")
	if _, ok := store.Get(context.Background(), "k"); !ok {
		t.Fatal("setup: value not stored")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel() // the client has gone

	if _, ok := store.Get(cancelled, "k"); ok {
		t.Error("Get served a request whose context was already cancelled; the caller's " +
			"cancellation is not reaching the store")
	}
	store.Set(cancelled, "k2", "v2")
	if _, ok := store.Get(context.Background(), "k2"); ok {
		t.Error("Set ran for a context that was already cancelled")
	}
}

// The cap still applies when the caller has no deadline of its own, so a hung
// server cannot hold a request open indefinitely.
func TestRedisCapsAnUnboundedCaller(t *testing.T) {
	store, _ := newTestRedis(t, time.Minute)
	defer store.Close()

	ctx, cancel := opContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("an operation built from a deadline-free caller has no deadline of its own")
	}
	if d := time.Until(deadline); d <= 0 || d > redisOpTimeout+time.Second {
		t.Fatalf("deadline is %v away, want it capped at %v", d, redisOpTimeout)
	}
}

// A caller with a TIGHTER deadline keeps it: the cap is a ceiling, not a floor.
func TestRedisDoesNotExtendATighterCallerDeadline(t *testing.T) {
	tight, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	ctx, cancel2 := opContext(tight)
	defer cancel2()
	deadline, _ := ctx.Deadline()
	if d := time.Until(deadline); d > time.Second {
		t.Fatalf("the caller's 10ms deadline was extended to %v; the op timeout is a "+
			"ceiling, not a replacement", d)
	}
}
