package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
