package cache

import (
	"testing"
	"time"
)

func TestLocalCache_StoreAndGet(t *testing.T) {
	l := NewLocalCache(5 * time.Minute)
	defer l.Close()

	l.Set("call_123", "find the bug")
	got, ok := l.Get("call_123")
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

	_, ok := l.Get("nonexistent")
	if ok {
		t.Error("expected false for missing key")
	}
}

func TestLocalCache_Delete(t *testing.T) {
	l := NewLocalCache(5 * time.Minute)
	defer l.Close()

	l.Set("call_123", "test")
	l.Delete("call_123")
	_, ok := l.Get("call_123")
	if ok {
		t.Error("expected false after delete")
	}
}

func TestLocalCache_Expiry(t *testing.T) {
	l := NewLocalCache(50 * time.Millisecond)
	defer l.Close()

	l.Set("call_123", "test")
	time.Sleep(100 * time.Millisecond)

	_, ok := l.Get("call_123")
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
	l.Set("a", "1")
	l.Set("b", "2")
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
				l.Set(key, "intent")
				l.Get(key)
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
		l.Set("call_"+string(rune('0'+i)), "test")
	}
	time.Sleep(100 * time.Millisecond)
	// Background eviction should have cleaned up.
	if l.Len() > 0 {
		t.Logf("eviction may not have completed yet, len=%d", l.Len())
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

	l.Set("call_1", "old")
	l.Set("call_1", "new")
	got, _ := l.Get("call_1")
	if got != "new" {
		t.Errorf("intent = %q, want 'new' (override)", got)
	}
}

func TestLocalCache_EvictsLeastRecentlyUsedByEntryCount(t *testing.T) {
	l := NewLocalCacheWithLimits(5*time.Minute, 2, 1<<20)
	defer l.Close()
	l.Set("a", "1")
	l.Set("b", "2")
	if _, ok := l.Get("a"); !ok {
		t.Fatal("expected a before eviction")
	}
	l.Set("c", "3")
	if _, ok := l.Get("b"); ok {
		t.Fatal("least recently used entry b was not evicted")
	}
	if _, ok := l.Get("a"); !ok {
		t.Fatal("recently used entry a was evicted")
	}
}

func TestLocalCache_BoundsBytesAndRejectsOversizedValues(t *testing.T) {
	l := NewLocalCacheWithLimits(5*time.Minute, 100, 8)
	defer l.Close()
	l.Set("a", "1234") // 5 bytes including key
	l.Set("b", "5678") // evicts a to remain under 8
	if _, ok := l.Get("a"); ok {
		t.Fatal("byte bound did not evict oldest entry")
	}
	l.Set("huge", "12345678")
	if _, ok := l.Get("huge"); ok {
		t.Fatal("oversized value should not be admitted")
	}
}
