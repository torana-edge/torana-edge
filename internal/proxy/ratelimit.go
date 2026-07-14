package proxy

import (
	"sync"
	"time"
)

type Limiter struct {
	mu           sync.Mutex
	tokens       float64
	lastRefill   time.Time
	active       int
}

type RateLimiter struct {
	mu      sync.Mutex
	limits  map[string]*Limiter
	rpm     int
	maxConn int
}

func NewRateLimiter(rpm, maxConn int) *RateLimiter {
	return &RateLimiter{
		limits:  make(map[string]*Limiter),
		rpm:     rpm,
		maxConn: maxConn,
	}
}

func (rl *RateLimiter) getLimiter(identity string) *Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if l, exists := rl.limits[identity]; exists {
		return l
	}
	l := &Limiter{
		tokens:     float64(rl.rpm),
		lastRefill: time.Now(),
	}
	rl.limits[identity] = l
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
	if l.active > 0 {
		l.active--
	}
}
