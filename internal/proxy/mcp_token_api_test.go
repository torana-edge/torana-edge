package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/mcpauth"
	"github.com/torana-edge/torana-edge/internal/pluginstate"
	"github.com/torana-edge/torana-edge/internal/secret"
)

func TestMCPTokensOperatorGuardAndRotation(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	sealer, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{mcpTokens: mcpauth.New(state, sealer)}
	handler := s.controlPlaneGuard(s.handleMCPToken)
	request := func(method, path, body, remote, origin string, local bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
		r.RemoteAddr = remote
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if local {
			r.Header.Set("X-Torana-Local-Request", "1")
		}
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	for _, tc := range []struct {
		method, body, remote, origin string
		local                        bool
		want                         int
	}{
		{http.MethodGet, "", "127.0.0.1:1234", "", false, 405},
		{http.MethodPost, "{}", "192.0.2.1:1234", "", true, 403},
		{http.MethodPost, "{}", "127.0.0.1:1234", "https://evil.example", true, 403},
		{http.MethodPost, "{}", "127.0.0.1:1234", "", false, 403},
		{http.MethodPost, "null", "127.0.0.1:1234", "", true, 400},
		{http.MethodPost, "{\"rotate\":true}", "127.0.0.1:1234", "", true, 400},
		{http.MethodPost, "{} {}", "127.0.0.1:1234", "", true, 400},
	} {
		w := request(tc.method, mcpTokenAPIPath, tc.body, tc.remote, tc.origin, tc.local)
		if w.Code != tc.want {
			t.Fatalf("%s response status: got %d, want %d", tc.method, w.Code, tc.want)
		}
		if token, err := s.mcpTokens.Current(); err != nil || token != "" {
			t.Fatal("rejected request provisioned a token or storage failed")
		}
	}
	readToken := func(path string) string {
		w := request(http.MethodPost, path, "{}", "127.0.0.1:1234", "", true)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("token response status/cache policy: %d", w.Code)
		}
		var result struct{ Token string }
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Token == "" {
			t.Fatal("invalid token response")
		}
		return result.Token
	}
	w := request(http.MethodPost, mcpTokenAPIPath, "{}", "127.0.0.1:1234", "", true)
	if w.Code != http.StatusConflict {
		t.Fatalf("unconfigured read status=%d", w.Code)
	}
	if token, err := s.mcpTokens.Current(); err != nil || token != "" {
		t.Fatal("token read provisioned a credential")
	}
	first := readToken(mcpTokenAPIPath + "/setup")
	if first != readToken(mcpTokenAPIPath) {
		t.Fatal("setup replaced an existing token")
	}
	rotated := readToken(mcpTokenAPIPath + "/rotate")
	if first == rotated || rotated != readToken(mcpTokenAPIPath) {
		t.Fatal("rotation did not replace the current token")
	}
}

func TestMCPTokenUnavailableFailsClosed(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.handleMCPToken(w, httptest.NewRequest(http.MethodPost, mcpTokenAPIPath, strings.NewReader("{}")))
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), `"token":`) {
		t.Fatal("unavailable storage did not fail closed")
	}
}
