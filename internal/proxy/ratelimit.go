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
	// Start the janitor even while limits are disabled: limits may be enabled
	// later by a config reload.
	go rl.janitor()
	return rl
}

// Update replaces the live limits without dropping in-flight concurrency
// accounting. Existing buckets retain their active count; their token balance
// is clamped to the new bucket capacity. Enabling RPM starts a full bucket.
func (rl *RateLimiter) Update(rpm, maxConn int) {
	if rl == nil {
		return
	}
	rl.mu.Lock()
	previousRPM := rl.rpm
	rl.rpm = rpm
	rl.maxConn = maxConn
	now := time.Now()
	for _, l := range rl.limits {
		l.mu.Lock()
		if rpm == 0 {
			l.tokens = 0
		} else if previousRPM <= 0 || l.tokens > float64(rpm) {
			l.tokens = float64(rpm)
		}
		l.lastRefill = now
		l.lastSeen = now
		l.mu.Unlock()
	}
	rl.mu.Unlock()
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

func (rl *RateLimiter) getLimiterLocked(key string) *Limiter {
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

// acquireLease binds release to this admission, not a later bucket for the
// same identity. In particular, an uncounted disabled admission must not
// release a request admitted after enforcement is enabled.
func (rl *RateLimiter) acquireLease(identity string) (func(), bool) {
	l, ok := rl.acquire(identity)
	if l == nil {
		return func() {}, ok
	}
	return sync.OnceFunc(func() { releaseLimiter(l) }), ok
}

func (rl *RateLimiter) acquire(identity string) (*Limiter, bool) {
	if rl == nil {
		return nil, true
	}
	rl.mu.Lock()
	rpm, maxConn := rl.rpm, rl.maxConn
	key := hashIdentity(identity)
	l := rl.limits[key]
	if rpm > 0 || maxConn > 0 {
		l = rl.getLimiterLocked(key)
	}
	if l == nil {
		rl.mu.Unlock()
		return nil, true
	}
	l.mu.Lock()
	rl.mu.Unlock()
	defer l.mu.Unlock()
	// Existing buckets continue tracking while enforcement is disabled;
	// brand-new identities do not allocate buckets on that path.
	l.lastSeen = time.Now()

	// Concurrency check
	if maxConn > 0 && l.active >= maxConn {
		return nil, false
	}

	// RPM check
	if rpm > 0 {
		now := time.Now()
		elapsed := now.Sub(l.lastRefill).Seconds()
		rate := float64(rpm) / 60.0
		l.tokens += elapsed * rate
		if l.tokens > float64(rpm) {
			l.tokens = float64(rpm)
		}
		l.lastRefill = now

		if l.tokens < 1.0 {
			return nil, false
		}
		l.tokens -= 1.0
	}

	l.active++
	return l, true
}

func releaseLimiter(l *Limiter) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastSeen = time.Now()
	if l.active > 0 {
		l.active--
	}
}
