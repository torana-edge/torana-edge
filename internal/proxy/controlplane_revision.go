package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// A process-specific keyed revision covers the full configuration, including
// secrets, without exposing an offline password oracle. Restarting invalidates
// snapshots too. Call the precondition check while holding mutationMu.
func (s *Server) configRevision(cfg provider.Config) string {
	b, _ := json.Marshal(cfg)
	h := hmac.New(sha256.New, []byte(s.controlPlaneRevisionKey))
	_, _ = h.Write(b)
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`
}

func (s *Server) checkConfigRevision(w http.ResponseWriter, r *http.Request) bool {
	// Every client must read before writing. There is no compatibility escape
	// hatch before the first release: absent headers must not mean last-write-wins.
	revision := r.Header.Get("If-Match")
	if revision == "" {
		http.Error(w, "If-Match is required; read configuration or plugins and send its ETag before writing", http.StatusPreconditionRequired)
		return false
	}
	// Require the exact snapshot token, not a wildcard, weak tag, or tag list.
	if len(r.Header.Values("If-Match")) != 1 || revision != s.configRevision(s.GetConfig().Providers) {
		http.Error(w, "configuration changed; read a fresh snapshot before retrying", http.StatusPreconditionFailed)
		return false
	}
	return true
}
