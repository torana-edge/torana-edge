package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

func TestQueryCredentialRateIdentity(t *testing.T) {
	identity := func(query string) string {
		r, _ := http.NewRequest(http.MethodPost, "https://provider.invalid/?"+query, nil)
		return callerCredentialsFrom(r).rateIdentity()
	}
	a := identity("key=alice")
	b := identity("key=bob")
	if a == "" || b == "" || a == b {
		t.Fatal("query callers share an identity")
	}
	if a != identity("alt=sse&k%65y=%61lice") {
		t.Fatal("escaping or ordinary parameters changed identity")
	}
	if a == identity("api_key=alice") {
		t.Fatal("query schemes are not domain-separated")
	}
	if identity("key=alice&key=second") == a {
		t.Fatal("repeated credentials were discarded")
	}
	rl := NewRateLimiter(0, 1)
	defer rl.Close()
	if !rl.Acquire(a) {
		t.Fatal("first caller refused")
	}
	defer rl.Release(a)
	if !rl.Acquire(b) {
		t.Fatal("second caller shared the first caller's concurrency bucket")
	}
	defer rl.Release(b)
	if rl.Acquire(a) {
		rl.Release(a)
		t.Fatal("same caller escaped its concurrency limit")
	}
}

func TestQueryCredentialsDiscardAmbiguousSemicolonComponents(t *testing.T) {
	for _, mode := range []string{"caller", "credential", "none"} {
		r, _ := http.NewRequest(http.MethodPost, "https://provider.invalid/?alt=sse;key=secret&key=other;alt=json&keep=a%3Bb", nil)
		caller := callerCredentialsFrom(r)
		err := applyProviderCredential(context.Background(), r, provider.Provider{Auth: provider.ProviderAuth{Mode: mode}}, caller,
			func(context.Context, string) ([]byte, error) { return []byte("managed"), nil })
		if err != nil {
			t.Fatal(err)
		}
		if r.URL.RawQuery != "keep=a%3Bb" {
			t.Errorf("%s: ambiguous query survived: %q", mode, r.URL.RawQuery)
		}
	}
}

func TestPluginEgressPreservesFunctionalQueryFields(t *testing.T) {
	const query = "key=document-id&api_key=field-name&access_token=field&keep=a%3Bb"
	seen := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()
	cfg := provider.DefaultConfig()
	cfg.Providers = map[string]provider.Provider{"test": {URL: upstream.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}}}
	cfg.Plugins.Runtime.Egress = map[string]provider.EgressBudget{"test-plugin": {MaxCallsPerMinute: 10}}
	srv, err := New(Config{Providers: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())
	_, refusal := send(t, srv, "test-plugin", egressPayload(t, "test", "/v1/chat/completions?"+query))
	if refusal != nil {
		t.Fatalf("egress refused: %v", refusal)
	}
	select {
	case got := <-seen:
		if got != query {
			t.Fatalf("plugin query changed to %q", got)
		}
	default:
		t.Fatal("plugin request never reached upstream")
	}
}

// Authentication in a URL must obey the same policy as authentication headers,
// including escaped names and repeated keys. Unrelated query bytes stay exact.
func TestQueryCredentialsRespectProviderAuth(t *testing.T) {
	const ordinary = "alt=sse&api-version=2026-01-01&opaque=a%20b%2fc"
	const credentials = "k%65y=caller&key=second&api_key=third&api-key=fourth&access_token=fifth"
	for _, mode := range []string{"caller", "credential", "none"} {
		t.Run(mode, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "https://provider.invalid/infer?"+ordinary+"&"+credentials, nil)
			caller := callerCredentialsFrom(req)
			err := applyProviderCredential(context.Background(), req, provider.Provider{Format: "gemini", Auth: provider.ProviderAuth{Mode: mode, Credential: "managed"}}, caller,
				func(context.Context, string) ([]byte, error) { return []byte("managed-secret"), nil })
			if err != nil {
				t.Fatal(err)
			}
			want := ordinary
			if mode == "caller" {
				want += "&" + credentials
			}
			if req.URL.RawQuery != want {
				t.Fatalf("query = %q, want %q", req.URL.RawQuery, want)
			}
			wantHeader := ""
			if mode == "credential" {
				wantHeader = "managed-secret"
			}
			if req.Header.Get("X-Goog-Api-Key") != wantHeader {
				t.Fatal("wrong managed credential header")
			}
		})
	}
}

func TestQueryCredentialsRestoreIngressOnFailover(t *testing.T) {
	for _, mode := range []string{"caller", "credential", "none"} {
		t.Run(mode, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "https://primary.invalid/infer?alt=sse&key=caller", strings.NewReader("{}"))
			rs := &reqState{CallerCredentials: callerCredentialsFrom(req)}
			ctx := context.WithValue(req.Context(), reqStateKey{}, rs)
			ctx = context.WithValue(ctx, routeContextKey{}, &RouteContext{ProviderName: "primary", StrippedPath: "/infer"})
			req = req.WithContext(ctx)
			// Only the ingress snapshot may supply caller authentication.
			req.URL.RawQuery = "alt=sse&key=primary-secret"
			calls := 0
			tr := &failoverRoundTripper{
				cfg: func() provider.Config {
					return provider.Config{Providers: map[string]provider.Provider{
						"primary": {URL: "https://primary.invalid", Fallback: []string{"backup"}},
						"backup":  {URL: "https://backup.invalid", Auth: provider.ProviderAuth{Mode: mode, Credential: "backup"}},
					}}
				},
				resolveCredential: func(context.Context, string) ([]byte, error) { return []byte("backup-secret"), nil },
				base: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					status := http.StatusTooManyRequests
					if calls == 2 {
						status = http.StatusOK
						want := "alt=sse"
						if mode == "caller" {
							want += "&key=caller"
						}
						if r.URL.RawQuery != want {
							t.Errorf("fallback query = %q, want %q", r.URL.RawQuery, want)
						}
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
				}),
			}
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if calls != 2 {
				t.Fatalf("calls = %d, want 2", calls)
			}
		})
	}
}

func TestQueryCredentialsRemovedOnPluginRoute(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://primary.invalid/infer?key=caller&alt=sse", nil)
	rs := &reqState{CallerCredentials: callerCredentialsFrom(req)}
	ctx := context.WithValue(req.Context(), reqStateKey{}, rs)
	rc := &RouteContext{ProviderName: "primary", StrippedPath: "/infer"}
	req = req.WithContext(context.WithValue(ctx, routeContextKey{}, rc))
	cfg := provider.Config{Providers: map[string]provider.Provider{
		"local": {URL: "http://local.invalid", Format: "gemini", Auth: provider.ProviderAuth{Mode: "none"}},
	}}
	s := &Server{}
	s.applyRoute(req, &engine.ChatRequest{}, "gemini", "primary", &wasm.RouteVerdict{Provider: "local"}, cfg)
	if req.URL.Host != "local.invalid" || req.URL.RawQuery != "alt=sse" {
		t.Fatalf("routed URL = %s", req.URL)
	}
}
