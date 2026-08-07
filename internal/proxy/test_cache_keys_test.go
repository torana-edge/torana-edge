package proxy

import "strconv"

// observerCacheKey independently reproduces the host-owned private-cache
// framing for the test-observer fixture. Proxy tests inspect the backing store
// to prove whether the guest hook ran; looking up the guest's logical key would
// become vacuous now that ordinary cache entries are plugin-private.
func observerCacheKey(key string) string {
	const pluginName = "test-observer"
	return "private\x00" + strconv.Itoa(len(pluginName)) + "\x00" + pluginName + key
}
