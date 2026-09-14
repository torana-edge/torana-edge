package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// Configuring a bridge selects inference-only routing even when both contracts
// are identical. The content-conversion fast path must not imply that auxiliary
// APIs are passed through. Exercise the real handler, not just MatchesPath.
func TestBridgeAuxiliaryBoundary(t *testing.T) {
	for i, clientProtocol := range bridgeProtocols {
		for _, mode := range []string{"native", "same_contract", "cross_contract"} {
			t.Run(string(clientProtocol)+"/"+mode, func(t *testing.T) {
				var upstreamCalls atomic.Int32
				const upstreamBody = `{"auxiliary":"passed through"}`
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upstreamCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, upstreamBody)
				}))
				t.Cleanup(upstream.Close)
				p := provider.Provider{URL: upstream.URL, Format: clientProtocol.Format(), Auth: provider.ProviderAuth{Mode: "none"}}
				if mode != "native" {
					upstreamProtocol := clientProtocol
					if mode == "cross_contract" {
						upstreamProtocol = bridgeProtocols[(i+1)%len(bridgeProtocols)]
					}
					p.Format = upstreamProtocol.Format()
					p.Bridge = &provider.BridgeConfig{Client: clientProtocol, Upstream: upstreamProtocol, Model: "aliased-model"}
					if p.Format == "gemini-codeassist" {
						p.Bridge.Project = "test-project"
					}
				}
				srv, err := New(Config{Providers: provider.Config{Providers: map[string]provider.Provider{"p": p}}})
				if err != nil {
					t.Fatal(err)
				}
				proxy := httptest.NewServer(srv.Handler())
				t.Cleanup(func() { proxy.Close(); _ = srv.Shutdown(context.Background()) })
				client := &http.Client{Timeout: 5 * time.Second}
				for _, endpoint := range []struct{ method, path string }{
					{http.MethodGet, "/v1/models"},
					{http.MethodPost, "/v1/messages/count_tokens"},
					{http.MethodGet, "/v1/responses/response-id"},
				} {
					t.Run(endpoint.method+endpoint.path, func(t *testing.T) {
						before := upstreamCalls.Load()
						req, err := http.NewRequest(endpoint.method, proxy.URL+"/provider/p"+endpoint.path, strings.NewReader(`{"model":"client-model"}`))
						if err != nil {
							t.Fatal(err)
						}
						req.Header.Set("Content-Type", "application/json")
						resp, err := client.Do(req)
						if err != nil {
							t.Fatal(err)
						}
						defer resp.Body.Close()
						body, err := io.ReadAll(resp.Body)
						if err != nil {
							t.Fatal(err)
						}
						if mode == "native" {
							if resp.StatusCode != http.StatusOK || string(body) != upstreamBody || upstreamCalls.Load()-before != 1 {
								t.Fatalf("native auxiliary request was not passed through: status=%d calls=%d body=%s", resp.StatusCode, upstreamCalls.Load()-before, body)
							}
							return
						}
						if resp.StatusCode != http.StatusBadRequest || upstreamCalls.Load() != before {
							t.Fatalf("bridge must reject before upstream: status=%d calls=%d body=%s", resp.StatusCode, upstreamCalls.Load()-before, body)
						}
						if !json.Valid(body) || !strings.Contains(string(body), "auxiliary APIs on an inference bridge") {
							t.Fatalf("expected a structured auxiliary-API error, got %s", body)
						}
					})
				}
			})
		}
	}
}
