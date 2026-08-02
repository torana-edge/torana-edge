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
	return httpRequestParityServerWith(t, []string{"test-http-server"})
}

// httpRequestParityServerWith loads the given fixtures (defaulting to the
// single test-http-server) into one real pipeline.
func httpRequestParityServerWith(t *testing.T, order []string) *Server {
	t.Helper()
	for _, name := range order {
		if _, err := os.Stat(fixturesDir + "/" + name + "/plugin.wasm"); err != nil {
			if os.Getenv("TORANA_E2E") != "" {
				t.Fatalf("%s fixture is not built; run make testdata: %v", name, err)
			}
			t.Skip(name + " fixture is not built; run make testdata")
		}
	}
	config := provider.DefaultConfig()
	config.Port = 8080
	config.Plugins = provider.PluginsConfig{
		Dir:             fixturesDir,
		Order:           order,
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

// headerEcho decodes the "headers" member of a fixture echo.
func headerEcho(t *testing.T, echo map[string]any) map[string][]string {
	t.Helper()
	raw, err := json.Marshal(echo["headers"])
	if err != nil {
		t.Fatalf("marshal echoed headers: %v", err)
	}
	var headers map[string][]string
	if err := json.Unmarshal(raw, &headers); err != nil {
		t.Fatalf("decode echoed headers: %v", err)
	}
	return headers
}

func assertHeaders(t *testing.T, got map[string][]string, want map[string][]string, absent ...string) {
	t.Helper()
	for name, values := range want {
		gotV, ok := got[name]
		if !ok {
			t.Errorf("header %q missing: got %v", name, got)
			continue
		}
		if len(gotV) != len(values) {
			t.Errorf("header %q values = %v, want %v", name, gotV, values)
			continue
		}
		for i := range values {
			if gotV[i] != values[i] {
				t.Errorf("header %q values = %v, want %v", name, gotV, values)
				break
			}
		}
	}
	for _, name := range absent {
		if _, ok := got[name]; ok {
			t.Errorf("header %q must be absent, got %v", name, got)
		}
	}
}

// TestPluginRouteHeaderPolicy — H1/H3/H8/H9: the plugin route forwards only
// the safe operational headers (canonical names, multi-values preserved)
// without the grant; Cookie, custom secrets, X-Torana-Agent, and credentials
// never arrive.
func TestPluginRouteHeaderPolicy(t *testing.T) {
	server := httpRequestParityServerWith(t, []string{"test-http-server-nogrant"})
	handler := server.Handler()

	// test-http-server-nogrant does NOT hold env.request_headers, so this
	// route row proves the operational set arrives grant-free.
	req := localControlPlaneRequest(http.MethodGet,
		"/_torana/plugin/test-http-server-nogrant/echo?x=1", nil)
	req.RemoteAddr = "127.0.0.1:4321"
	req.Header = http.Header{
		"Accept":            {"text/html"},
		"content-type":      {"application/json", "charset=utf-8"}, // mixed case + multi-value
		"User-Agent":        {"curl/8"},
		"Authorization":     {"Bearer sk-torana-secret"},
		"X-Api-Key":         {"sk-torana-apikey"},
		"X-Torana-User":     {"alice"},
		"Cookie":            {"session=secret"},
		"X-Customer-Secret": {"s3cr3t"},
		"X-Torana-Agent":    {"spoofed"},
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got := headerEcho(t, decodeEcho(t, rec.Body.Bytes()))
	assertHeaders(t, got,
		map[string][]string{
			"Accept":       {"text/html"},
			"Content-Type": {"application/json", "charset=utf-8"},
			"User-Agent":   {"curl/8"},
		},
		"Authorization", "X-Api-Key", "X-Torana-User",
		"Cookie", "X-Customer-Secret", "X-Torana-Agent",
	)
}

// TestPluginRouteHeaderPolicyWithGrant — H2: when the exact target plugin
// holds the approved env.request_headers grant, the five credential/identity
// headers arrive too; everything else still never does.
func TestPluginRouteHeaderPolicyWithGrant(t *testing.T) {
	server := httpRequestParityServer(t)
	handler := server.Handler()

	req := localControlPlaneRequest(http.MethodGet,
		"/_torana/plugin/test-http-server/echo?x=1", nil)
	req.RemoteAddr = "127.0.0.1:4321"
	req.Header = http.Header{
		"Accept":            {"text/html"},
		"Authorization":     {"Bearer sk-torana-secret"},
		"X-Api-Key":         {"sk-torana-apikey"},
		"X-Torana-User":     {"alice"},
		"X-Torana-Team":     {"team-a"},
		"X-Torana-Tenant":   {"tenant-a"},
		"Cookie":            {"session=secret"},
		"X-Customer-Secret": {"s3cr3t"},
		"X-Torana-Agent":    {"spoofed"},
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got := headerEcho(t, decodeEcho(t, rec.Body.Bytes()))
	assertHeaders(t, got,
		map[string][]string{
			"Accept":          {"text/html"},
			"Authorization":   {"Bearer sk-torana-secret"},
			"X-Api-Key":       {"sk-torana-apikey"},
			"X-Torana-User":   {"alice"},
			"X-Torana-Team":   {"team-a"},
			"X-Torana-Tenant": {"tenant-a"},
		},
		"Cookie", "X-Customer-Secret", "X-Torana-Agent",
	)
}

// TestAgentRouteHeaderPolicy — the agent route applies the SAME policy:
// operational headers without the grant, credentials with it, everything
// else never.
func TestAgentRouteHeaderPolicy(t *testing.T) {
	server := httpRequestParityServerWith(t, []string{"test-http-server-nogrant"})
	handler := server.Handler()

	// test-http-server-nogrant serves the same /status operation without the
	// env.request_headers grant.
	req := localControlPlaneRequest(http.MethodGet,
		"/_torana/api/v1/agent/plugins/test-http-server-nogrant/status", nil)
	req.RemoteAddr = "127.0.0.1:4321"
	req.Header = http.Header{
		"Accept":         {"application/json"},
		"User-Agent":     {"curl/8"},
		"Authorization":  {"Bearer sk-torana-secret"},
		"Cookie":         {"session=secret"},
		"X-Torana-Agent": {"spoofed"},
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got := headerEcho(t, decodeEcho(t, rec.Body.Bytes()))
	assertHeaders(t, got,
		map[string][]string{
			"Accept":     {"application/json"},
			"User-Agent": {"curl/8"},
		},
		"Authorization", "Cookie", "X-Torana-Agent",
	)
}

// TestHTTPHeaderPolicySamePipelineIsolation — H6: under one pinned pipeline,
// a granted target receives the sensitive headers and an ungranted target in
// the SAME pipeline never does.
func TestHTTPHeaderPolicySamePipelineIsolation(t *testing.T) {
	config := provider.DefaultConfig()
	config.Port = 8080
	config.Plugins = provider.PluginsConfig{
		Dir:             fixturesDir,
		Order:           []string{"test-http-server", "test-http-server-nogrant"},
		AllowUnapproved: true,
	}
	server, err := New(Config{Port: "8080", Providers: config})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer server.Shutdown(context.Background())
	handler := server.Handler()

	rawHeaders := http.Header{
		"Accept":        {"text/html"},
		"Authorization": {"Bearer sk-torana-secret"},
	}
	for name, tc := range map[string]struct {
		target string
		want   map[string][]string
	}{
		"granted target": {
			target: "test-http-server",
			want: map[string][]string{
				"Accept":        {"text/html"},
				"Authorization": {"Bearer sk-torana-secret"},
			},
		},
		"ungranted target": {
			target: "test-http-server-nogrant",
			want: map[string][]string{
				"Accept": {"text/html"},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := localControlPlaneRequest(http.MethodGet,
				"/_torana/plugin/"+tc.target+"/echo", nil)
			req.RemoteAddr = "127.0.0.1:4321"
			req.Header = rawHeaders
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			got := headerEcho(t, decodeEcho(t, rec.Body.Bytes()))
			assertHeaders(t, got, tc.want, "X-Customer-Secret")
		})
	}
}
