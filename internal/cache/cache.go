// Package cache provides the intent cache interface and implementations.
// The local memory implementation uses sync.RWMutex with TTL eviction.
// In future, a Redis-backed implementation can be added by implementing
// the same IntentCache interface.
package cache

import (
	"sync"
	"time"
)

// IntentCache stores tool-call intents keyed by tool_use_id.
// Implementations must be safe for concurrent use.
type IntentCache interface {
	// Store saves an intent for a tool call ID.
	Store(toolUseID, intent string)

	// Get retrieves a cached intent. Returns false if not found or expired.
	Get(toolUseID string) (string, bool)

	// Delete removes a cached intent.
	Delete(toolUseID string)

	// Len returns the number of cached entries (including expired).
	Len() int
}

// ── Local memory implementation ────────────────────────────────────

// LocalCache is an in-memory intent cache with TTL-based eviction.
// A background goroutine periodically cleans expired entries.
type LocalCache struct {
	mu      sync.RWMutex
	entries map[string]cacheEntry
	ttl     time.Duration

	// Eviction control.
	stopCh chan struct{}
	doneCh chan struct{}
}

type cacheEntry struct {
	intent    string
	expiresAt time.Time
}

// NewLocalCache creates a LocalCache with the given TTL. A background
// goroutine evicts expired entries every ttl/2 interval.
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

func (l *LocalCache) Store(toolUseID, intent string) {
	l.mu.Lock()
	l.entries[toolUseID] = cacheEntry{
		intent:    intent,
		expiresAt: time.Now().Add(l.ttl),
	}
	l.mu.Unlock()
}

func (l *LocalCache) Get(toolUseID string) (string, bool) {
	l.mu.RLock()
	e, ok := l.entries[toolUseID]
	l.mu.RUnlock()
	if !ok {
		return "", false
	}
	if time.Now().After(e.expiresAt) {
		l.Delete(toolUseID)
		return "", false
	}
	return e.intent, true
}

func (l *LocalCache) Delete(toolUseID string) {
	l.mu.Lock()
	delete(l.entries, toolUseID)
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
	select {
	case <-l.stopCh:
		// Already closed.
	default:
		close(l.stopCh)
	}
	<-l.doneCh
}

// evictLoop periodically scans and removes expired entries.
func (l *LocalCache) evictLoop() {
	defer close(l.doneCh)
	ticker := time.NewTicker(l.ttl / 2)
	if l.ttl/2 < time.Second {
		ticker = time.NewTicker(time.Second)
	}
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
