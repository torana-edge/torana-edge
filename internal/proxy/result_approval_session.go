package proxy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const approvalSessionPath = "/_torana/api/v1/approval-session"
const approvalSessionCookie = "torana-approval-session"
const approvalSessionHeader = "X-Torana-Approval-Session"

// This is a CSRF/explicit-interaction safeguard, not authentication against an
// unrestricted same-user shell. Issue #467 remains a release blocker for human
// exceptions. Session tokens never enter agent discovery, MCP, or plugin input.
func (s *Server) approvalSessionMAC(payload string) string {
	h := hmac.New(sha256.New, []byte(s.controlPlaneRevisionKey))
	h.Write([]byte("result-approval-session\x00" + payload))
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Server) handleApprovalSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeAgentError(w, 405, "method_not_allowed", "Create a review session with POST.")
		return
	}
	payload := strconv.FormatInt(time.Now().Add(15*time.Minute).Unix(), 10) + "." + rand.Text()
	token := payload + "." + s.approvalSessionMAC(payload)
	http.SetCookie(w, &http.Cookie{Name: approvalSessionCookie, Value: token, Path: "/_torana/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 900})
	w.Header().Set("Cache-Control", "no-store")
	writeAgentJSON(w, 200, map[string]string{"token": token})
}

func (s *Server) validApprovalSession(r *http.Request) bool {
	cookie, err := r.Cookie(approvalSessionCookie)
	token := r.Header.Get(approvalSessionHeader)
	if err != nil || len(token) > 256 || !hmac.Equal([]byte(cookie.Value), []byte(token)) {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[1]) < 16 {
		return false
	}
	expires, err := strconv.ParseInt(parts[0], 10, 64)
	now := time.Now().Unix()
	if err != nil || expires <= now || expires > now+900 {
		return false
	}
	return hmac.Equal([]byte(parts[2]), []byte(s.approvalSessionMAC(parts[0]+"."+parts[1])))
}
