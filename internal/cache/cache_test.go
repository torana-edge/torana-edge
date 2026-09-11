package cache

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLocalCache_StoreAndGet(t *testing.T) {
	l := NewLocalCache(5 * time.Minute)
	defer l.Close()

	l.Set(context.Background(), "call_123", "find the bug")
	got, ok := l.Get(context.Background(), "call_123")
	if !ok {
		t.Fatal("expected intent to be found")
	}
	if got != "find the bug" {
		t.Errorf("intent = %q, want %q", got, "find the bug")
	}
}

func TestLocalCache_Missing(t *testing.T) {
	l := NewLocalCache(5 * time.Minute)
	defer l.Close()

	_, ok := l.Get(context.Background(), "nonexistent")
	if ok {
		t.Error("expected false for missing key")
	}
}

func TestLocalCache_Delete(t *testing.T) {
	l := NewLocalCache(5 * time.Minute)
	defer l.Close()

	l.Set(context.Background(), "call_123", "test")
	l.Delete(context.Background(), "call_123")
	_, ok := l.Get(context.Background(), "call_123")
	if ok {
		t.Error("expected false after delete")
	}
}

func TestLocalCache_Expiry(t *testing.T) {
	l := NewLocalCache(50 * time.Millisecond)
	defer l.Close()

	l.Set(context.Background(), "call_123", "test")
	time.Sleep(100 * time.Millisecond)

	_, ok := l.Get(context.Background(), "call_123")
	if ok {
		t.Error("expected expiry after TTL")
	}
}

func TestLocalCache_Len(t *testing.T) {
	l := NewLocalCache(5 * time.Minute)
	defer l.Close()

	if l.Len() != 0 {
		t.Errorf("initial len = %d, want 0", l.Len())
	}
	l.Set(context.Background(), "a", "1")
	l.Set(context.Background(), "b", "2")
	if l.Len() != 2 {
		t.Errorf("len = %d, want 2", l.Len())
	}
}

func TestLocalCache_Concurrent(t *testing.T) {
	l := NewLocalCache(5 * time.Minute)
	defer l.Close()

	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func(id int) {
			for j := 0; j < 100; j++ {
				key := "call_" + string(rune('0'+id))
				l.Set(context.Background(), key, "intent")
				l.Get(context.Background(), key)
				l.Len()
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestLocalCache_EvictCleanup(t *testing.T) {
	l := NewLocalCache(20 * time.Millisecond)
	defer l.Close()

	for i := 0; i < 10; i++ {
		l.Set(context.Background(), "call_"+string(rune('0'+i)), "test")
	}
	deadline := time.Now().Add(2 * time.Second)
	for l.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := l.Len(); got != 0 {
		t.Fatalf("background eviction left %d expired entries", got)
	}
}

func TestLocalCache_CloseIdempotent(t *testing.T) {
	l := NewLocalCache(5 * time.Minute)
	l.Close()
	l.Close() // should not panic
}

func TestLocalCache_Override(t *testing.T) {
	l := NewLocalCache(5 * time.Minute)
	defer l.Close()

	l.Set(context.Background(), "call_1", "old")
	l.Set(context.Background(), "call_1", "new")
	got, _ := l.Get(context.Background(), "call_1")
	if got != "new" {
		t.Errorf("intent = %q, want 'new' (override)", got)
	}
}

func TestLocalCache_EvictsLeastRecentlyUsedByEntryCount(t *testing.T) {
	l := NewLocalCacheWithLimits(5*time.Minute, 2, 1<<20)
	defer l.Close()
	l.Set(context.Background(), "a", "1")
	l.Set(context.Background(), "b", "2")
	if _, ok := l.Get(context.Background(), "a"); !ok {
		t.Fatal("expected a before eviction")
	}
	l.Set(context.Background(), "c", "3")
	if _, ok := l.Get(context.Background(), "b"); ok {
		t.Fatal("least recently used entry b was not evicted")
	}
	if _, ok := l.Get(context.Background(), "a"); !ok {
		t.Fatal("recently used entry a was evicted")
	}
}

func TestLocalCache_BoundsBytesAndRejectsOversizedValues(t *testing.T) {
	l := NewLocalCacheWithLimits(5*time.Minute, 100, 8)
	defer l.Close()
	l.Set(context.Background(), "a", "1234") // 5 bytes including key
	l.Set(context.Background(), "b", "5678") // evicts a to remain under 8
	if _, ok := l.Get(context.Background(), "a"); ok {
		t.Fatal("byte bound did not evict oldest entry")
	}
	l.Set(context.Background(), "huge", "12345678")
	if _, ok := l.Get(context.Background(), "huge"); ok {
		t.Fatal("oversized value should not be admitted")
	}
}

// A write that cannot be admitted must leave the store as it found it.
//
// Set removed the existing entry and THEN checked whether the new value fitted,
// so an oversized write deleted a perfectly good cached value and inserted
// nothing. The caller has no way to see it: Set returns nothing, and the next
// Get is simply a miss — indistinguishable from ordinary eviction.
func TestSetRejectingAnOversizedValueKeepsTheExistingOne(t *testing.T) {
	ctx := context.Background()
	l := NewLocalCacheWithLimits(time.Minute, 0, 64) // 64-byte ceiling
	defer l.Close()

	l.Set(ctx, "k", "small")
	if got, ok := l.Get(ctx, "k"); !ok || got != "small" {
		t.Fatalf("setup: Get = %q,%v", got, ok)
	}

	l.Set(ctx, "k", strings.Repeat("x", 200)) // far past the ceiling

	got, ok := l.Get(ctx, "k")
	if !ok {
		t.Fatal("the existing value was deleted by a write that was then rejected; " +
			"a rejected write must not empty the slot it was aiming at")
	}
	if got != "small" {
		t.Fatalf("value = %q, want the original %q", got, "small")
	}
}

// The same for a key that did not exist: a rejected write inserts nothing and
// leaves no trace.
func TestSetRejectingAnOversizedValueInsertsNothing(t *testing.T) {
	ctx := context.Background()
	l := NewLocalCacheWithLimits(time.Minute, 0, 64)
	defer l.Close()

	l.Set(ctx, "absent", strings.Repeat("x", 200))
	if _, ok := l.Get(ctx, "absent"); ok {
		t.Fatal("an oversized value was admitted")
	}
	if n := l.Len(); n != 0 {
		t.Fatalf("Len = %d after a rejected write, want 0", n)
	}
}
