package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// httpRequestParityServer loads the test-http-server fixture into a real
// pipeline, like TestPluginAgentOperationDispatch does. The fixture echoes the
// forwarded HttpRequest fields back as JSON (plugin route: /echo; agent route:
// /agent/status), so a test can assert the host forwarded the request
// faithfully at the real transport boundary without reflecting raw input into
// HTML.
func httpRequestParityServer(t *testing.T) *Server {
	t.Helper()
	if _, err := os.Stat(fixturesDir + "/test-http-server/plugin.wasm"); err != nil {
		if os.Getenv("TORANA_E2E") != "" {
			t.Fatalf("test-http-server fixture is not built; run make testdata: %v", err)
		}
		t.Skip("test-http-server fixture is not built; run make testdata")
	}
	config := provider.DefaultConfig()
	config.Port = 8080
	config.Plugins = provider.PluginsConfig{
		Dir:             fixturesDir,
		Order:           []string{"test-http-server"},
		AllowUnapproved: true,
	}
	server, err := New(Config{Port: "8080", Providers: config})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { server.Shutdown(context.Background()) })
	return server
}

// decodeEcho decodes a fixture JSON echo body.
func decodeEcho(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var echo map[string]any
	if err := json.Unmarshal(body, &echo); err != nil {
		t.Fatalf("decode echo: %v (%s)", err, body)
	}
	return echo
}

// TestRequestSchemeDerivesFromTheConnection — the scheme is a property of the
// ACTUAL ACCEPTED CONNECTION, never of the caller's request target. An
// absolute-form request URI is caller-supplied and not proof of transport
// security; X-Forwarded-Proto is equally untrusted without a trusted-proxy
// boundary. The contradiction rows are the regressions: plaintext + absolute
// https still reports http, TLS + absolute http still reports https.
func TestRequestSchemeDerivesFromTheConnection(t *testing.T) {
	cases := []struct {
		name string
		req  *http.Request
		want string
	}{
		{"plain", &http.Request{}, "http"},
		{"tls", &http.Request{TLS: &tls.ConnectionState{}}, "https"},
		{"plain with https absolute target", &http.Request{URL: &url.URL{Scheme: "https"}}, "http"},
		{"tls with http absolute target", &http.Request{URL: &url.URL{Scheme: "http"}, TLS: &tls.ConnectionState{}}, "https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestScheme(tc.req); got != tc.want {
				t.Fatalf("requestScheme = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPluginRouteForwardsRequestFields — the /_torana/plugin/<name>/* builder
// forwards the raw query (no "?"), the connection-derived scheme, and the
// caller address to the plugin, at the real transport boundary. The scheme
// rows include the contradictions: a caller-asserted absolute https target
// over a plaintext connection still reports http, and an absolute http target
// over TLS still reports https.
func TestPluginRouteForwardsRequestFields(t *testing.T) {
	server := httpRequestParityServer(t)
	handler := server.Handler()

	for name, tc := range map[string]struct {
		target     string
		tls        bool
		wantQuery  string
		wantScheme string
	}{
		"plain http": {
			target:     "/_torana/plugin/test-http-server/echo?q=a%20b&x=1",
			wantQuery:  "q=a%20b&x=1",
			wantScheme: "http",
		},
		"tls https": {
			target:     "/_torana/plugin/test-http-server/echo?q=a%20b&x=1",
			tls:        true,
			wantQuery:  "q=a%20b&x=1",
			wantScheme: "https",
		},
		"plaintext with https absolute target stays http": {
			target:     "https://127.0.0.1/_torana/plugin/test-http-server/echo?x=1",
			wantQuery:  "x=1",
			wantScheme: "http",
		},
		"tls with http absolute target stays https": {
			target:     "http://127.0.0.1/_torana/plugin/test-http-server/echo?x=1",
			tls:        true,
			wantQuery:  "x=1",
			wantScheme: "https",
		},
		"no query": {
			target:     "/_torana/plugin/test-http-server/echo",
			wantQuery:  "",
			wantScheme: "http",
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := localControlPlaneRequest(http.MethodGet, tc.target, nil)
			req.RemoteAddr = "127.0.0.1:4321"
			// tc.tls is the single source of truth for the CONNECTION state:
			// httptest.NewRequest fakes a TLS state for absolute https
			// targets, which would make the contradiction rows meaningless.
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			} else {
				req.TLS = nil
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			echo := decodeEcho(t, rec.Body.Bytes())
			if echo["query"] != tc.wantQuery {
				t.Errorf("query = %q, want %q", echo["query"], tc.wantQuery)
			}
			if echo["scheme"] != tc.wantScheme {
				t.Errorf("scheme = %q, want %q", echo["scheme"], tc.wantScheme)
			}
			if echo["remote_addr"] != "127.0.0.1:4321" {
				t.Errorf("remote_addr = %q, want 127.0.0.1:4321", echo["remote_addr"])
			}
		})
	}
}

// TestAgentRouteForwardsRequestFields — the /_torana/api/v1/agent/<op>
// builder forwards the same six request fields with the same semantics as the
// plugin route: one connection-derived scheme vocabulary, raw query, caller
// address. Contradiction rows included, as for the plugin route.
func TestAgentRouteForwardsRequestFields(t *testing.T) {
	server := httpRequestParityServer(t)
	handler := server.Handler()

	for name, tc := range map[string]struct {
		target     string
		tls        bool
		wantQuery  string
		wantScheme string
	}{
		"plain http": {
			target:     "/_torana/api/v1/agent/plugins/test-http-server/status?since=7&full=1",
			wantQuery:  "since=7&full=1",
			wantScheme: "http",
		},
		"tls https": {
			target:     "/_torana/api/v1/agent/plugins/test-http-server/status?since=7&full=1",
			tls:        true,
			wantQuery:  "since=7&full=1",
			wantScheme: "https",
		},
		"plaintext with https absolute target stays http": {
			target:     "https://127.0.0.1/_torana/api/v1/agent/plugins/test-http-server/status?since=7&full=1",
			wantQuery:  "since=7&full=1",
			wantScheme: "http",
		},
		"tls with http absolute target stays https": {
			target:     "http://127.0.0.1/_torana/api/v1/agent/plugins/test-http-server/status?since=7&full=1",
			tls:        true,
			wantQuery:  "since=7&full=1",
			wantScheme: "https",
		},
		"no query": {
			target:     "/_torana/api/v1/agent/plugins/test-http-server/status",
			wantQuery:  "",
			wantScheme: "http",
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := localControlPlaneRequest(http.MethodGet, tc.target, nil)
			req.RemoteAddr = "127.0.0.1:4321"
			// tc.tls is the single source of truth for the CONNECTION state:
			// httptest.NewRequest fakes a TLS state for absolute https
			// targets, which would make the contradiction rows meaningless.
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			} else {
				req.TLS = nil
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			echo := decodeEcho(t, rec.Body.Bytes())
			if echo["query"] != tc.wantQuery {
				t.Errorf("query = %q, want %q", echo["query"], tc.wantQuery)
			}
			if echo["scheme"] != tc.wantScheme {
				t.Errorf("scheme = %q, want %q", echo["scheme"], tc.wantScheme)
			}
			if echo["remote_addr"] != "127.0.0.1:4321" {
				t.Errorf("remote_addr = %q, want 127.0.0.1:4321", echo["remote_addr"])
			}
		})
	}
}
