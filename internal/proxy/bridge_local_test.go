package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/bridge"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestBridgeBodyLimitUsesClientEnvelope(t *testing.T) {
	for _, client := range bridgeProtocols {
		t.Run(string(client), func(t *testing.T) {
			proxy, _ := newBridgeProxy(t, client, bridge.OpenAIChat, func(http.ResponseWriter, *http.Request) { t.Error("oversized body reached upstream") })
			status, raw, headers := callBridge(t, proxy, client, strings.Repeat("x", maxBodySize+1), false)
			if status != http.StatusRequestEntityTooLarge || headers.Get("Content-Type") != "application/json" || !json.Valid(raw) || !strings.Contains(string(raw), "request body too large") {
				t.Fatalf("status=%d headers=%v body=%s", status, headers, raw)
			}
		})
	}
}

func TestBridgeLocalRateLimitHasNoUpstreamOutcome(t *testing.T) {
	for _, client := range bridgeProtocols {
		t.Run(string(client), func(t *testing.T) {
			limiter := NewRateLimiter(0, 1)
			defer limiter.Close()
			release, admitted := limiter.acquireLease("default")
			if !admitted {
				t.Fatal("initial admission refused")
			}
			defer release()
			rs := &reqState{AuditUpstreamRequestBytes: 200}
			ctx := context.WithValue(context.Background(), reqStateKey{}, rs)
			ctx = context.WithValue(ctx, bridgeContextKey{}, &bridgeExchange{Client: client, Upstream: bridge.OpenAIChat})
			req := httptest.NewRequest(http.MethodPost, "http://backend.invalid/v1/chat/completions", strings.NewReader("{}")).WithContext(ctx)
			transport := &failoverRoundTripper{
				cfg:         func() provider.Config { return provider.Config{} },
				rateLimiter: limiter,
				base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
					t.Fatal("rate-limited request reached upstream")
					return nil, nil
				}),
			}
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 429 || !rs.Synthetic || rs.UpstreamStatus != 0 || rs.AuditUpstreamRequestBytes != 0 || rs.AuditErrorCode != "local_rate_limit" || rs.chatResponse("", "", nil, "").UpstreamStatus != 0 {
				t.Fatalf("local attribution: status=%d state=%+v", resp.StatusCode, rs)
			}
			if !json.Valid(raw) || !strings.Contains(string(raw), "Torana request rate limit exceeded") {
				t.Fatalf("error envelope: %s", raw)
			}
		})
	}
}

func TestBridgeAnthropicUsageKeepsSourceAccounting(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			proxy, _ := newBridgeProxy(t, bridge.OpenAIChat, bridge.Anthropic, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", "upstream-representation")
				w.Header().Set("Content-Digest", "upstream-digest")
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					raw := strings.Replace(bridgeUpstreamSSE(bridge.Anthropic, false), `"input_tokens":7,"cache_read_input_tokens":3`, `"input_tokens":10,"cache_read_input_tokens":20`, 1)
					_, _ = io.WriteString(w, raw+"\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					raw := strings.Replace(bridgeUpstreamJSON(bridge.Anthropic, false), `"input_tokens":10`, `"input_tokens":10,"cache_read_input_tokens":20`, 1)
					_, _ = io.WriteString(w, raw)
				}
			})
			body := bridgeClientBody(bridge.OpenAIChat, false, streaming)
			if streaming {
				body = bridgeStreamingClientBody(bridge.OpenAIChat, false)
			}
			status, raw, headers := callBridge(t, proxy, bridge.OpenAIChat, body, streaming)
			if status != 200 || !strings.Contains(string(raw), `"prompt_tokens":30`) {
				t.Fatalf("translated usage: status=%d body=%s", status, raw)
			}
			if headers.Get("ETag") != "" || headers.Get("Content-Digest") != "" {
				t.Fatalf("stale validators: %v", headers)
			}
			stats := statsSnapshot(t, proxy.URL)
			if stats["total_tokens_in"] != float64(10) || stats["total_tokens_out"] != float64(3) || stats["total_cache_read_tokens"] != float64(20) || stats["total_cache_write_tokens"] != float64(0) {
				t.Fatalf("source accounting: %v", stats)
			}
		})
	}
}
