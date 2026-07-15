// Package cache provides a TTL-evicting key-value store used by the WASM
// runtime for cross-request plugin state (extracted intents, compacted tool
// results — keyed by tool_call_id). The local memory implementation uses
// sync.RWMutex with background TTL eviction; a Redis-backed implementation
// can be added later behind the same Store interface.
//
// Resurrected from the pre-WASM-migration intent cache (a2ca3e3) that fixed
// "intent cache grows forever" (torana-edge#6).
package cache

import (
	"sync"
	"time"
)

// Store is a TTL key-value store safe for concurrent use.
type Store interface {
	// Set saves a value under key, resetting its TTL.
	Set(key, value string)

	// Get retrieves a value. Returns false if not found or expired.
	Get(key string) (string, bool)

	// Delete removes an entry.
	Delete(key string)

	// Len returns the number of entries (including not-yet-evicted expired ones).
	Len() int

	// Close releases background resources (the eviction goroutine).
	Close()
}

// ── Local memory implementation ────────────────────────────────────

// LocalCache is an in-memory Store with TTL-based eviction.
// A background goroutine periodically cleans expired entries.
type LocalCache struct {
	mu      sync.RWMutex
	entries map[string]cacheEntry
	ttl     time.Duration

	// Eviction control.
	stopCh    chan struct{}
	doneCh    chan struct{}
	closeOnce sync.Once
}

type cacheEntry struct {
	value     string
	expiresAt time.Time
}

// NewLocalCache creates a LocalCache with the given TTL. A background
// goroutine evicts expired entries every ttl/2 (at least every second).
// Call Close() to stop the eviction goroutine.
func NewLocalCache(ttl time.Duration) *LocalCache {
	l := &LocalCache{
		entries: make(map[string]cacheEntry),
		ttl:     ttl,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
	go l.evictLoop()
	return l
}

func (l *LocalCache) Set(key, value string) {
	l.mu.Lock()
	l.entries[key] = cacheEntry{
		value:     value,
		expiresAt: time.Now().Add(l.ttl),
	}
	l.mu.Unlock()
}

func (l *LocalCache) Get(key string) (string, bool) {
	l.mu.RLock()
	e, ok := l.entries[key]
	l.mu.RUnlock()
	if !ok {
		return "", false
	}
	if time.Now().After(e.expiresAt) {
		l.Delete(key)
		return "", false
	}
	return e.value, true
}

func (l *LocalCache) Delete(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

func (l *LocalCache) Len() int {
	l.mu.RLock()
	n := len(l.entries)
	l.mu.RUnlock()
	return n
}

// Close stops the eviction goroutine. Safe to call multiple times.
func (l *LocalCache) Close() {
	l.closeOnce.Do(func() {
		close(l.stopCh)
	})
	<-l.doneCh
}

// evictLoop periodically scans and removes expired entries.
func (l *LocalCache) evictLoop() {
	defer close(l.doneCh)
	interval := l.ttl / 2
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stopCh:
			return
		case <-ticker.C:
			l.evict()
		}
	}
}

func (l *LocalCache) evict() {
	now := time.Now()
	l.mu.Lock()
	for id, e := range l.entries {
		if now.After(e.expiresAt) {
			delete(l.entries, id)
		}
	}
	l.mu.Unlock()
}
