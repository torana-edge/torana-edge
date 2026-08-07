package proxy

import "github.com/torana-edge/torana-edge/internal/wasm"

// observerCacheKey asks the host where the test-observer fixture's private
// cache entry lands. Proxy tests inspect the backing store to prove whether the
// guest hook ran, and the guest's logical key is not the stored key now that
// ordinary cache entries are plugin-private.
//
// This delegates rather than reproducing the framing. A copy stays correct only
// until the framing moves, and then fails as though the fixture had broken —
// which is exactly what happened to the coordinated intent probes in
// internal/plugin when the shared domain was introduced.
func observerCacheKey(key string) string {
	return wasm.PrivateCacheKey("test-observer", key)
}
