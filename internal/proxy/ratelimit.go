package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// limiterIdleTTL is how long an identity's limiter may sit unused (with no
// active requests) before the janitor evicts it.
const limiterIdleTTL = 10 * time.Minute

type Limiter struct {
	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time
	lastSeen   time.Time
	active     int
}

type RateLimiter struct {
	mu      sync.Mutex
	limits  map[string]*Limiter
	rpm     int
	maxConn int

	stopJanitor chan struct{}
	janitorOnce sync.Once
}

func NewRateLimiter(rpm, maxConn int) *RateLimiter {
	rl := &RateLimiter{
		limits:      make(map[string]*Limiter),
		rpm:         rpm,
		maxConn:     maxConn,
		stopJanitor: make(chan struct{}),
	}
	if rpm > 0 || maxConn > 0 {
		go rl.janitor()
	}
	return rl
}

// Close stops the background janitor. Safe to call multiple times.
func (rl *RateLimiter) Close() {
	if rl == nil {
		return
	}
	rl.janitorOnce.Do(func() { close(rl.stopJanitor) })
}

// janitor evicts limiters idle for limiterIdleTTL with no active requests,
// bounding memory for long-running processes with many distinct callers.
func (rl *RateLimiter) janitor() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stopJanitor:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-limiterIdleTTL)
			rl.mu.Lock()
			for id, l := range rl.limits {
				l.mu.Lock()
				idle := l.active == 0 && l.lastSeen.Before(cutoff)
				l.mu.Unlock()
				if idle {
					delete(rl.limits, id)
				}
			}
			rl.mu.Unlock()
		}
	}
}

// hashIdentity keys the limiter map by a digest, never the raw credential —
// storing API keys as long-lived map keys would retain secrets in memory.
func hashIdentity(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:16])
}

func (rl *RateLimiter) getLimiter(identity string) *Limiter {
	key := hashIdentity(identity)
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if l, exists := rl.limits[key]; exists {
		return l
	}
	l := &Limiter{
		tokens:     float64(rl.rpm),
		lastRefill: time.Now(),
		lastSeen:   time.Now(),
	}
	rl.limits[key] = l
	return l
}

func (rl *RateLimiter) Acquire(identity string) bool {
	if rl == nil {
		return true
	}
	if rl.rpm <= 0 && rl.maxConn <= 0 {
		return true // limits disabled
	}

	l := rl.getLimiter(identity)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastSeen = time.Now()

	// Concurrency check
	if rl.maxConn > 0 && l.active >= rl.maxConn {
		return false
	}

	// RPM check
	if rl.rpm > 0 {
		now := time.Now()
		elapsed := now.Sub(l.lastRefill).Seconds()
		rate := float64(rl.rpm) / 60.0
		l.tokens += elapsed * rate
		if l.tokens > float64(rl.rpm) {
			l.tokens = float64(rl.rpm)
		}
		l.lastRefill = now

		if l.tokens < 1.0 {
			return false
		}
		l.tokens -= 1.0
	}

	l.active++
	return true
}

func (rl *RateLimiter) Release(identity string) {
	if rl == nil {
		return
	}
	if rl.rpm <= 0 && rl.maxConn <= 0 {
		return
	}

	l := rl.getLimiter(identity)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastSeen = time.Now()
	if l.active > 0 {
		l.active--
	}
}
