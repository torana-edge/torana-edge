// Package cache provides a TTL-evicting key-value store used by the WASM
// runtime for cross-request plugin state (extracted intents, compacted tool
// results). The local implementation combines TTL expiry with entry/byte-bounded
// LRU eviction; Redis uses the same Store interface and server-side eviction.
//
// Both bounds matter: a cache that only expires by TTL grows without limit
// under a workload that keeps writing new keys.
package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Store is a TTL key-value store safe for concurrent use.
//
// Every operation takes the CALLER's context. A Store may be backed by a
// network service, and a cache lookup on behalf of a request the client has
// already abandoned should stop when that request does — without a context
// there is nothing to stop it with, and the work is charged to a request
// nobody is waiting for.
type Store interface {
	// Set saves a value under key, resetting its TTL.
	Set(ctx context.Context, key, value string)

	// Get retrieves a value. Returns false if not found or expired.
	Get(ctx context.Context, key string) (string, bool)

	// Delete removes an entry.
	Delete(ctx context.Context, key string)

	// Len returns the number of entries (including not-yet-evicted expired ones).
	Len() int

	// Close releases background resources (the eviction goroutine).
	Close()
}

// ── Local memory implementation ────────────────────────────────────

// LocalCache is an in-memory Store with TTL-based eviction.
// A background goroutine periodically cleans expired entries.
type LocalCache struct {
	mu         sync.Mutex
	entries    map[string]*cacheEntry
	lru        *list.List
	ttl        time.Duration
	maxEntries int
	maxBytes   int
	bytes      int

	// Eviction control.
	stopCh    chan struct{}
	doneCh    chan struct{}
	closeOnce sync.Once
}

type cacheEntry struct {
	key       string
	value     string
	expiresAt time.Time
	size      int
	element   *list.Element
}

// NewLocalCache creates a LocalCache with the given TTL. A background
// goroutine evicts expired entries every ttl/2 (at least every second).
// Call Close() to stop the eviction goroutine.
func NewLocalCache(ttl time.Duration) *LocalCache {
	return NewLocalCacheWithLimits(ttl, DefaultMaxEntries, DefaultMaxBytes)
}

// NewLocalCacheWithLimits creates a TTL cache with LRU admission bounds.
// Values larger than maxBytes are not admitted.
func NewLocalCacheWithLimits(ttl time.Duration, maxEntries, maxBytes int) *LocalCache {
	l := &LocalCache{
		entries:    make(map[string]*cacheEntry),
		lru:        list.New(),
		ttl:        ttl,
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
	go l.evictLoop()
	return l
}

// The context is accepted to satisfy Store and deliberately ignored: these
// operations are a map lookup under a mutex, with nothing to cancel.
func (l *LocalCache) Set(_ context.Context, key, value string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Size FIRST. Removing the old entry and then rejecting the new value
	// deleted a perfectly good cached value because its replacement happened
	// to be too large — a write that cannot be admitted must leave the store
	// as it found it, not empty the slot it was aiming at.
	size := len(key) + len(value)
	if l.maxBytes > 0 && size > l.maxBytes {
		return
	}
	if old := l.entries[key]; old != nil {
		l.removeLocked(old)
	}
	e := &cacheEntry{
		key:       key,
		value:     value,
		expiresAt: time.Now().Add(l.ttl),
		size:      size,
	}
	e.element = l.lru.PushFront(e)
	l.entries[key] = e
	l.bytes += size
	l.evictBoundsLocked()
}

func (l *LocalCache) Get(_ context.Context, key string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		return "", false
	}
	if time.Now().After(e.expiresAt) {
		l.removeLocked(e)
		return "", false
	}
	l.lru.MoveToFront(e.element)
	return e.value, true
}

func (l *LocalCache) Delete(_ context.Context, key string) {
	l.mu.Lock()
	if e := l.entries[key]; e != nil {
		l.removeLocked(e)
	}
	l.mu.Unlock()
}

func (l *LocalCache) Len() int {
	l.mu.Lock()
	n := len(l.entries)
	l.mu.Unlock()
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
	for _, e := range l.entries {
		if now.After(e.expiresAt) {
			l.removeLocked(e)
		}
	}
	l.mu.Unlock()
}

func (l *LocalCache) evictBoundsLocked() {
	for (l.maxEntries > 0 && len(l.entries) > l.maxEntries) ||
		(l.maxBytes > 0 && l.bytes > l.maxBytes) {
		back := l.lru.Back()
		if back == nil {
			return
		}
		l.removeLocked(back.Value.(*cacheEntry))
	}
}

func (l *LocalCache) removeLocked(e *cacheEntry) {
	delete(l.entries, e.key)
	l.lru.Remove(e.element)
	l.bytes -= e.size
}
