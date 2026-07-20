package plugin_sdk

import "testing"

func TestContentAddressedCacheKey(t *testing.T) {
	a := ContentAddressedCacheKey("compacted", "call-1", "content", "intent", "policy-v1")
	if a != ContentAddressedCacheKey("compacted", "call-1", "content", "intent", "policy-v1") {
		t.Fatal("same inputs must produce the same key")
	}
	if a == ContentAddressedCacheKey("compacted", "call-1", "changed", "intent", "policy-v1") {
		t.Fatal("changed content must invalidate the cache key")
	}
	// Length framing avoids the classic ["ab", "c"] / ["a", "bc"] collision.
	if ContentAddressedCacheKey("n", "ab", "c") == ContentAddressedCacheKey("n", "a", "bc") {
		t.Fatal("input boundaries must affect the key")
	}
}
