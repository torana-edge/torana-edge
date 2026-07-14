package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"strings"

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
			"primary": {URL: failingBackend.URL, Fallback: []string{"fallback1"}},
			"fallback1": {URL: failingBackend.URL},
		},
	}

	frt := &failoverRoundTripper{
		base: http.DefaultTransport,
		cfg: func() provider.Config { return cfg },
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
