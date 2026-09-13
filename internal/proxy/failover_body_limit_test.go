package proxy

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// Ingress limits apply before rewriting. A plugin or host rewrite can grow
// an accepted body past the retry buffer limit; never send a consumed tail.
func TestFailoverRejectsOversizedOutgoingBody(t *testing.T) {
	for _, size := range []int{maxBodySize, maxBodySize + 1, maxBodySize + 2} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			limiter := NewRateLimiter(0, 1)
			defer limiter.Close()
			calls := 0
			payload := strings.Repeat("x", size)
			tr := &failoverRoundTripper{
				rateLimiter: limiter,
				cfg: func() provider.Config {
					return provider.Config{Providers: map[string]provider.Provider{
						"primary": {URL: "https://primary.invalid", Fallback: []string{"backup"}},
						"backup":  {URL: "https://backup.invalid"},
					}}
				},
				base: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					if string(body) != payload {
						t.Errorf("transport received %d of %d bytes", len(body), size)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
				}),
			}
			rs := &reqState{CompactionRequestPrepared: true}
			ctx := context.WithValue(context.Background(), reqStateKey{}, rs)
			ctx = context.WithValue(ctx, routeContextKey{}, &RouteContext{ProviderName: "primary", Identity: "caller"})
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://primary.invalid/infer", strings.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := tr.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if size > maxBodySize {
				if err == nil || resp != nil || calls != 0 {
					t.Fatalf("oversized body: err=%v response=%v upstream calls=%d", err, resp, calls)
				}
				if rs.CompactionReportsCommitted || rs.CompactionRequestPrepared {
					t.Fatal("unsent request retained compaction accounting")
				}
			} else if err != nil || calls != 1 {
				t.Fatalf("boundary-sized body: err=%v calls=%d", err, calls)
			}
			if !limiter.Acquire("caller") {
				t.Fatal("concurrency slot leaked")
			}
			limiter.Release("caller")
		})
	}
}
