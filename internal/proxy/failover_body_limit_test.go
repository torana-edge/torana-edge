package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/provider"
)

type retryTrackedBody struct {
	io.Reader
	closes int
}

func (b *retryTrackedBody) Read(p []byte) (int, error) {
	if b.closes != 0 {
		return 0, fmt.Errorf("read after close")
	}
	return b.Reader.Read(p)
}
func (b *retryTrackedBody) Close() error { b.closes++; return nil }

// Adding a fallback must not change which body sizes reach the primary.
// Bodies beyond the retry buffer stream intact once; bodies at the limit
// remain replayable. Cover known and unknown lengths and ownership of Close.
func TestFailoverPreservesOversizedOutgoingBody(t *testing.T) {
	for _, size := range []int{maxBodySize, maxBodySize + 1, maxBodySize + 2} {
		for _, fallback := range []bool{false, true} {
			for _, unknownLength := range []bool{false, true} {
				t.Run(fmt.Sprintf("size=%d/fallback=%t/unknown=%t", size, fallback, unknownLength), func(t *testing.T) {
					logs := captureLogs(t)
					limiter := NewRateLimiter(0, 1)
					defer limiter.Close()
					payload := strings.Repeat("x", size)
					original := &retryTrackedBody{Reader: strings.NewReader(payload)}
					calls := 0
					tr := &failoverRoundTripper{
						rateLimiter: limiter,
						cfg: func() provider.Config {
							var backups []string
							if fallback {
								backups = []string{"backup"}
							}
							return provider.Config{Providers: map[string]provider.Provider{
								"primary": {URL: "https://primary.invalid", Fallback: backups},
								"backup":  {URL: "https://backup.invalid"},
							}}
						},
						base: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
							calls++
							body, err := io.ReadAll(r.Body)
							_ = r.Body.Close()
							if err != nil {
								t.Fatal(err)
							}
							if string(body) != payload {
								t.Fatalf("received %d of %d bytes", len(body), size)
							}
							status := http.StatusTooManyRequests
							if calls == 2 {
								status = http.StatusOK
							}
							return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
						}),
					}
					rs := &reqState{CompactionRequestPrepared: true, AuditUpstreamRequestBytes: int64(size)}
					ctx := context.WithValue(context.Background(), reqStateKey{}, rs)
					ctx = context.WithValue(ctx, routeContextKey{}, &RouteContext{ProviderName: "primary", Identity: "caller"})
					req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://primary.invalid/infer", original)
					req.ContentLength = int64(size)
					if unknownLength {
						req.ContentLength = -1
					}
					resp, err := tr.RoundTrip(req)
					if err != nil {
						t.Fatal(err)
					}
					if fallback && size > maxBodySize && !strings.Contains(logs.String(), "outgoing body exceeds retry buffer limit") {
						t.Fatal("missing fallback-disabled diagnostic")
					}
					_ = resp.Body.Close()
					wantCalls, wantStatus := 1, http.StatusTooManyRequests
					if fallback && size <= maxBodySize {
						wantCalls, wantStatus = 2, http.StatusOK
					}
					if calls != wantCalls || resp.StatusCode != wantStatus {
						t.Fatalf("calls=%d status=%d, want %d/%d", calls, resp.StatusCode, wantCalls, wantStatus)
					}
					if original.closes != 1 {
						t.Fatalf("original closed %d times", original.closes)
					}
					if !rs.CompactionReportsCommitted || rs.AuditUpstreamRequestBytes != int64(size) {
						t.Fatal("sent request accounting was discarded")
					}
					if !limiter.Acquire("caller") {
						t.Fatal("concurrency slot leaked")
					}
					limiter.Release("caller")
				})
			}
		}
	}
}

func TestRetryBodyReadFailureHasLocalAuditAttribution(t *testing.T) {
	rs := &reqState{AuditUpstreamRequestBytes: 123, CompactionRequestPrepared: true}
	ctx := context.WithValue(context.Background(), reqStateKey{}, rs)
	ctx = context.WithValue(ctx, routeContextKey{}, &RouteContext{ProviderName: "primary"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://primary.invalid", &failingBody{data: []byte("partial")})
	tr := &failoverRoundTripper{
		cfg: func() provider.Config {
			return provider.Config{Providers: map[string]provider.Provider{"primary": {Fallback: []string{"backup"}}}}
		},
		base: roundTripperFunc(func(*http.Request) (*http.Response, error) { t.Fatal("upstream reached"); return nil, nil }),
	}
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("read error ignored")
	}
	record := rs.auditRecord(http.StatusBadGateway, nil)
	if record.UpstreamRequestBytes != 0 || record.ErrorCode != "request_body_read_failed" || record.Verdict != "host_error" {
		t.Fatalf("incorrect local failure audit: %+v", record)
	}
	if rs.CompactionRequestPrepared || rs.CompactionReportsCommitted {
		t.Fatal("unsent request credited")
	}
}
