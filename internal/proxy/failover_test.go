package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestFailoverExhaustion(t *testing.T) {
	// A backend that always returns 500
	failingBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed"}`))
	}))
	defer failingBackend.Close()

	cfg := provider.Config{
		Providers: map[string]provider.Provider{
			"primary":   {URL: failingBackend.URL, Fallback: []string{"fallback1"}},
			"fallback1": {URL: failingBackend.URL},
		},
	}

	frt := &failoverRoundTripper{
		base: http.DefaultTransport,
		cfg:  func() provider.Config { return cfg },
	}

	req, _ := http.NewRequest("POST", failingBackend.URL, strings.NewReader(`{}`))
	ctx := context.WithValue(req.Context(), routeContextKey{}, &RouteContext{
		ProviderName: "primary",
	})
	req = req.WithContext(ctx)

	resp, err := frt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", resp.StatusCode)
	}

	// Body should NOT be closed. We should be able to read it.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body (was it closed?): %v", err)
	}
	if string(body) != `{"error":"failed"}` {
		t.Errorf("expected body, got %q", string(body))
	}
}

// TestFailoverReleasesTokenOnTransportError: a transport error with no
// fallbacks must release the concurrency token. Regression: the token was
// only released via rateLimitBody.Close, which never wraps a nil response,
// so N connection errors permanently exhausted the identity's slots.
func TestFailoverReleasesTokenOnTransportError(t *testing.T) {
	rl := NewRateLimiter(0, 1) // one concurrent slot
	defer rl.Close()

	frt := &failoverRoundTripper{
		base:        http.DefaultTransport,
		cfg:         func() provider.Config { return provider.Config{} },
		rateLimiter: rl,
	}

	// 127.0.0.1:1 refuses connections — every attempt is a transport error.
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("POST", "http://127.0.0.1:1/v1/chat", strings.NewReader(`{}`))
		if _, err := frt.RoundTrip(req); err == nil {
			t.Fatalf("attempt %d: expected transport error", i)
		}
	}

	// With the leak, the single slot would now be gone and this returns false.
	if !rl.Acquire("default") {
		t.Fatal("concurrency token leaked after transport errors")
	}
	rl.Release("default")
}

// TestFailoverReleasesTokenOnRetryableStatus: a retryable status (500) with
// no fallbacks must wrap the body so Close releases the token.
func TestFailoverReleasesTokenOnRetryableStatus(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer backend.Close()

	rl := NewRateLimiter(0, 1)
	defer rl.Close()

	frt := &failoverRoundTripper{
		base:        http.DefaultTransport,
		cfg:         func() provider.Config { return provider.Config{} },
		rateLimiter: rl,
	}

	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("POST", backend.URL, strings.NewReader(`{}`))
		resp, err := frt.RoundTrip(req)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		resp.Body.Close()
	}

	if !rl.Acquire("default") {
		t.Fatal("concurrency token leaked after retryable statuses")
	}
	rl.Release("default")
}

func TestRateLimitBodyReleasesOnlyOnce(t *testing.T) {
	rl := NewRateLimiter(0, 1)
	defer rl.Close()
	if !rl.Acquire("caller") {
		t.Fatal("failed to acquire initial slot")
	}

	body := &rateLimitBody{
		ReadCloser:  io.NopCloser(strings.NewReader("ok")),
		identity:    "caller",
		rateLimiter: rl,
	}
	if err := body.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	// A duplicate close must not release a slot held by this second request.
	if !rl.Acquire("caller") {
		t.Fatal("failed to acquire replacement slot")
	}
	if rl.Acquire("caller") {
		t.Fatal("duplicate body close released the replacement request's slot")
	}
	rl.Release("caller")
}

func TestRateLimiterUpdateAppliesWithoutDroppingActiveRequests(t *testing.T) {
	rl := NewRateLimiter(0, 1)
	defer rl.Close()
	if !rl.Acquire("caller") {
		t.Fatal("failed to acquire initial slot")
	}

	rl.Update(0, 2)
	if !rl.Acquire("caller") {
		t.Fatal("updated concurrency limit was not applied")
	}
	if rl.Acquire("caller") {
		t.Fatal("update lost in-flight concurrency accounting")
	}
	rl.Release("caller")
	rl.Release("caller")
}

// Failover sends a request to a provider the caller never addressed. The
// credential rules that follow from that are the subject of these tests, and
// the last one is the reason they exist: until the fix, failover could not
// succeed at all, because the fallback rejected the caller's key.

// failoverEnv stands up a primary that always fails and a fallback that records
// what credential it was handed.
type failoverEnv struct {
	frt        *failoverRoundTripper
	seenAuth   string
	seenAPIKey string
	reached    bool
}

func newFailoverEnv(t *testing.T, fallbackKeyEnv string, fallbackAuthRequired string) *failoverEnv {
	t.Helper()
	env := &failoverEnv{}

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(primary.Close)

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.reached = true
		env.seenAuth = r.Header.Get("Authorization")
		env.seenAPIKey = r.Header.Get("X-Api-Key")
		// A real provider rejects a credential it did not issue.
		if fallbackAuthRequired != "" && env.seenAuth != "Bearer "+fallbackAuthRequired {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(fallback.Close)

	cfg := provider.Config{
		Providers: map[string]provider.Provider{
			"primary": {URL: primary.URL, Fallback: []string{"backup"}},
			"backup":  {URL: fallback.URL, APIKeyEnv: fallbackKeyEnv},
		},
	}
	env.frt = &failoverRoundTripper{
		base:          http.DefaultTransport,
		cfg:           func() provider.Config { return cfg },
		resolveSecret: func(envName, _ string) string { return os.Getenv(envName) },
	}
	return env
}

func (e *failoverEnv) roundTrip(t *testing.T, callerKey string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", "http://primary/v1/chat/completions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+callerKey)
	req.Header.Set("X-Api-Key", callerKey)
	req = req.WithContext(context.WithValue(req.Context(), routeContextKey{},
		&RouteContext{ProviderName: "primary", StrippedPath: "/v1/chat/completions"}))

	resp, err := e.frt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestFailoverDoesNotLeakCallerCredential is the security half. The caller
// issued that key to one vendor; a 429 from that vendor is not consent to send
// it to another.
func TestFailoverDoesNotLeakCallerCredential(t *testing.T) {
	t.Setenv("FAILOVER_BACKUP_KEY", "sk-backup")
	env := newFailoverEnv(t, "FAILOVER_BACKUP_KEY", "")

	env.roundTrip(t, "sk-caller-primary-only")

	if !env.reached {
		t.Fatal("fallback was never reached")
	}
	if strings.Contains(env.seenAuth, "sk-caller-primary-only") ||
		strings.Contains(env.seenAPIKey, "sk-caller-primary-only") {
		t.Errorf("the caller's credential was forwarded to a different vendor: auth=%q x-api-key=%q",
			env.seenAuth, env.seenAPIKey)
	}
	if env.seenAuth != "Bearer sk-backup" {
		t.Errorf("Authorization = %q, want the fallback's own key", env.seenAuth)
	}
}

// TestFailoverStripsCredentialWhenFallbackHasNone — an unauthenticated target
// (a local model server) must receive no credential at all, not the caller's.
func TestFailoverStripsCredentialWhenFallbackHasNone(t *testing.T) {
	env := newFailoverEnv(t, "", "")

	env.roundTrip(t, "sk-caller")

	if !env.reached {
		t.Fatal("fallback was never reached")
	}
	if env.seenAuth != "" || env.seenAPIKey != "" {
		t.Errorf("credential forwarded to a provider configured with none: auth=%q x-api-key=%q",
			env.seenAuth, env.seenAPIKey)
	}
}

// TestFailoverActuallySucceeds is the regression test that matters most, and it
// is the one that shows failover was never functional.
//
// The fallback here behaves like a real provider: it rejects a credential it
// did not issue. Before the fix the caller's key was forwarded verbatim, the
// fallback returned 401, and 401 is not retryable — so that 401 became the
// caller's response. A configured fallback made the outcome strictly worse than
// having none.
func TestFailoverActuallySucceeds(t *testing.T) {
	t.Setenv("FAILOVER_BACKUP_KEY", "sk-backup")
	env := newFailoverEnv(t, "FAILOVER_BACKUP_KEY", "sk-backup")

	resp := env.roundTrip(t, "sk-caller-primary-only")

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("failover returned %d (%s), want 200 — the fallback rejected the credential it was handed",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// TestFailoverStripsCredentialEvenWithoutAResolver is the fail-open guard. The
// strip used to live inside `if t.resolveSecret != nil`, so a nil resolver
// skipped it and the caller's key travelled to the fallback — the exact bug the
// PR set out to fix, reachable through the branch meant to fix it.
func TestFailoverStripsCredentialEvenWithoutAResolver(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://primary.example/v1/chat", nil)
	req.Header.Set("Authorization", "Bearer caller-secret")
	req.Header.Set("X-Api-Key", "caller-secret")

	applyProviderCredential(req, provider.Provider{URL: "https://fallback.example"}, "fb", "failover", nil)

	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("caller Authorization survived a nil resolver: %q", got)
	}
	if got := req.Header.Get("X-Api-Key"); got != "" {
		t.Errorf("caller X-Api-Key survived a nil resolver: %q", got)
	}
}

// TestForwardCallerCredentialIsOptIn covers the setup the strip would otherwise
// break: a fallback that is a second endpoint of the same vendor, or a local
// model server, where the caller's credential is the correct one to send.
// Both documented failover examples are that shape.
func TestForwardCallerCredentialIsOptIn(t *testing.T) {
	newReq := func() *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "https://primary.example/v1/chat", nil)
		r.Header.Set("Authorization", "Bearer caller-secret")
		r.Header.Set("X-Api-Key", "caller-secret")
		return r
	}

	t.Run("off by default", func(t *testing.T) {
		req := newReq()
		applyProviderCredential(req, provider.Provider{}, "fb", "failover", func(string, string) string { return "" })
		if req.Header.Get("Authorization") != "" {
			t.Error("caller credential forwarded without opting in")
		}
	})

	t.Run("forwarded when declared", func(t *testing.T) {
		req := newReq()
		applyProviderCredential(req, provider.Provider{ForwardCallerCredential: true}, "fb", "failover",
			func(string, string) string { return "" })
		if got := req.Header.Get("Authorization"); got != "Bearer caller-secret" {
			t.Errorf("Authorization = %q, want the caller's", got)
		}
		if got := req.Header.Get("X-Api-Key"); got != "caller-secret" {
			t.Errorf("X-Api-Key = %q, want the caller's", got)
		}
	})

	t.Run("the fallback's own key still wins", func(t *testing.T) {
		// Opting in must not override a credential the fallback declares:
		// that would send the wrong key to a provider that has its own.
		req := newReq()
		applyProviderCredential(req, provider.Provider{ForwardCallerCredential: true, APIKeyEnv: "FB_KEY"},
			"fb", "failover", func(string, string) string { return "fallback-secret" })
		if got := req.Header.Get("Authorization"); got != "Bearer fallback-secret" {
			t.Errorf("Authorization = %q, want the fallback's own key", got)
		}
	})
}
