package plugin_sdk

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// ContentAddressedCacheKey creates a stable cache key from every input that
// can affect a derived value. Length-prefixing prevents ambiguous joins. A
// compactor should include original content, intent, policy/version, and may
// include tool_call_id as one input; tool_call_id alone is not safe.
func ContentAddressedCacheKey(namespace string, inputs ...string) string {
	h := sha256.New()
	for _, input := range inputs {
		h.Write([]byte(strconv.Itoa(len(input))))
		h.Write([]byte{':'})
		h.Write([]byte(input))
	}
	return strings.TrimSuffix(namespace, ":") + ":sha256:" + hex.EncodeToString(h.Sum(nil))
}
