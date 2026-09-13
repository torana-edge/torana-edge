package proxy

// Legacy test probes for bucket capacity. Production requests must use
// acquireLease: releasing by identity cannot model configuration transitions.
func (rl *RateLimiter) Acquire(identity string) bool {
	_, ok := rl.acquire(identity)
	return ok
}

func (rl *RateLimiter) Release(identity string) {
	if rl == nil {
		return
	}
	rl.mu.Lock()
	l := rl.limits[hashIdentity(identity)]
	rl.mu.Unlock()
	releaseLimiter(l)
}
