package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/torana-edge/torana-edge/internal/auditlog"
	"github.com/torana-edge/torana-edge/internal/bridge"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestBridgeHonorsPluginStreamModeReplacement(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-mutator/plugin.wasm")
	for _, client := range bridgeProtocols {
		for _, originalStream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", client, originalStream), func(t *testing.T) {
				acceptedStream := !originalStream
				var hits atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					wantPath, wantQuery, wantAccept := "/v1beta/models/upstream-model:generateContent", "", "application/json"
					if acceptedStream {
						wantPath, wantQuery, wantAccept = "/v1beta/models/upstream-model:streamGenerateContent", "alt=sse", "text/event-stream"
					}
					if r.URL.Path != wantPath || r.URL.RawQuery != wantQuery || r.Header.Get("Accept") != wantAccept {
						t.Errorf("post-plugin transport: path=%q query=%q accept=%q", r.URL.Path, r.URL.RawQuery, r.Header.Get("Accept"))
					}
					w.Header().Set("Content-Type", wantAccept)
					if acceptedStream {
						_, _ = io.WriteString(w, bridgeUpstreamSSE(bridge.Gemini, false)+"\n\n")
					} else {
						_, _ = io.WriteString(w, bridgeUpstreamJSON(bridge.Gemini, false))
					}
				}))
				t.Cleanup(upstream.Close)
				cfg := provider.Config{
					Providers: map[string]provider.Provider{"p": {
						URL: upstream.URL, Format: "gemini", Auth: provider.ProviderAuth{Mode: "none"},
						Bridge: &provider.BridgeConfig{Client: client, Upstream: bridge.Gemini, Model: "upstream-model"},
					}},
					Plugins: provider.PluginsConfig{Dir: "../../examples/plugins", Order: []string{"test-mutator"}, AllowUnapproved: true},
				}
				srv, err := New(Config{Providers: cfg})
				if err != nil {
					t.Fatal(err)
				}
				proxy := httptest.NewServer(srv.Handler())
				t.Cleanup(func() { proxy.Close(); _ = srv.Shutdown(context.Background()) })
				body := strings.ReplaceAll(bridgeClientBody(client, false, originalStream), "look up weather", "toggle stream mode")
				status, raw, headers := callBridge(t, proxy, client, body, originalStream)
				if status != http.StatusOK || hits.Load() != 1 || !strings.Contains(string(raw), "It is sunny") {
					t.Fatalf("status=%d upstream_calls=%d body=%s", status, hits.Load(), raw)
				}
				if isEventStreamMediaType(headers.Get("Content-Type")) != acceptedStream {
					t.Fatalf("client response mode: %v", headers)
				}
				if acceptedStream {
					assertBridgeStreamWireTerminal(t, client, raw)
				} else if !json.Valid(raw) {
					t.Fatalf("invalid client JSON: %s", raw)
				}
			})
		}
	}
}

func TestBridgeGeminiModelPathEscapedOnce(t *testing.T) {
	for _, tc := range []struct{ model, escaped string }{
		{"gemini-2.5-pro", "gemini-2.5-pro"},
		{"gemini pro", "gemini%20pro"},
		{"model[1]", "model%5B1%5D"},
		{"model-é", "model-%C3%A9"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			var hits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				stream := hits.Add(1) == 2
				suffix, query := ":generateContent", ""
				if stream {
					suffix, query = ":streamGenerateContent", "?alt=sse"
				}
				want := "/deployment%20prefix/v1beta/models/" + tc.escaped + suffix + query
				if r.RequestURI != want || r.URL.Path != "/deployment prefix/v1beta/models/"+tc.model+suffix {
					t.Errorf("wire path=%q decoded=%q, want wire=%q", r.RequestURI, r.URL.Path, want)
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, bridgeUpstreamSSE(bridge.Gemini, false)+"\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, bridgeUpstreamJSON(bridge.Gemini, false))
				}
			}))
			t.Cleanup(upstream.Close)
			srv, err := New(Config{Providers: provider.Config{Providers: map[string]provider.Provider{"p": {
				URL: upstream.URL + "/deployment%20prefix", Format: "gemini", Auth: provider.ProviderAuth{Mode: "none"},
				Bridge: &provider.BridgeConfig{Client: bridge.OpenAIChat, Upstream: bridge.Gemini, Model: tc.model},
			}}}})
			if err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(srv.Handler())
			t.Cleanup(func() { proxy.Close(); _ = srv.Shutdown(context.Background()) })
			for _, stream := range []bool{false, true} {
				status, raw, _ := callBridge(t, proxy, bridge.OpenAIChat, bridgeClientBody(bridge.OpenAIChat, false, stream), stream)
				if status != http.StatusOK {
					t.Fatalf("status=%d body=%s", status, raw)
				}
			}
			if hits.Load() != 2 {
				t.Fatalf("upstream calls=%d", hits.Load())
			}
		})
	}
}

type bridgeDiagnosticBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *bridgeDiagnosticBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func (b *bridgeDiagnosticBody) Close() error { b.closed = true; return nil }

func TestBridgeUpstreamErrorDiagnosticsAreOptInAndBounded(t *testing.T) {
	for _, tc := range []struct {
		name, level, optIn string
		wantBody           bool
	}{
		{"default", "", "", false},
		{"debug", "debug", "", false},
		{"opt_in_without_debug", "", "1", false},
		{"debug_with_opt_in", "debug", "1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TORANA_LOG_LEVEL", tc.level)
			t.Setenv("TORANA_DEBUG_UPSTREAM_ERRORS", tc.optIn)
			var logs syncLogBuffer
			oldWriter := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(oldWriter) })
			body := &bridgeDiagnosticBody{Reader: strings.NewReader("max_tokens rejected\n" + strings.Repeat("x", maxBridgeErrorDiagnosticBytes) + "omitted-tail")}
			resp := &http.Response{StatusCode: 400, Header: http.Header{"Retry-After": []string{"3"}, "X-Api-Key": []string{"secret-header"}}, Body: body}
			ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ID: 42})
			bridgeUpstreamError(ctx, resp, &bridgeExchange{Client: bridge.OpenAIChat, Upstream: bridge.Anthropic}, "backend")
			defer resp.Body.Close()
			clientBody, _ := io.ReadAll(resp.Body)
			if !body.closed || resp.StatusCode != 400 || resp.Header.Get("Retry-After") != "3" || !json.Valid(clientBody) || strings.Contains(string(clientBody), "max_tokens") {
				t.Fatalf("error response/body ownership: closed=%t status=%d headers=%v body=%s", body.closed, resp.StatusCode, resp.Header, clientBody)
			}
			got := logs.String()
			if strings.Contains(got, "max_tokens") != tc.wantBody || strings.Contains(got, "secret-header") || strings.Contains(got, "omitted-tail") {
				t.Fatalf("unexpected diagnostic: %q", got)
			}
			if tc.wantBody {
				for _, part := range []string{"id=42", `provider="backend"`, "upstream=anthropic", "status=400", "body_truncated=true", `max_tokens rejected\n`} {
					if !strings.Contains(got, part) {
						t.Errorf("diagnostic missing %q", part)
					}
				}
				if body.read != maxBridgeErrorDiagnosticBytes+1 || strings.Count(got, "\n") != 1 {
					t.Fatalf("unbounded diagnostic: read=%d lines=%d", body.read, strings.Count(got, "\n"))
				}
			} else if body.read != 0 {
				t.Fatalf("read error body without opt-in: %d bytes", body.read)
			}
		})
	}
}

func TestBridgeUpstreamErrorDiagnosticsReachOperatorOnly(t *testing.T) {
	t.Setenv("TORANA_LOG_LEVEL", "debug")
	t.Setenv("TORANA_DEBUG_UPSTREAM_ERRORS", "1")
	var logs syncLogBuffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldWriter) })
	proxy, srv := newBridgeProxy(t, bridge.OpenAIChat, bridge.Anthropic, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"max_tokens exceeds model limit"}}`)
	})
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	writer, err := openAuditWriter(&auditlog.Config{Enabled: true, Path: auditPath})
	if err != nil {
		t.Fatal(err)
	}
	srv.swapAuditWriter(writer)
	status, raw, _ := callBridge(t, proxy, bridge.OpenAIChat, bridgeClientBody(bridge.OpenAIChat, false, false), false)
	proxy.Close() // Wait for the request's audit append before inspecting the file.
	if status != 400 || strings.Contains(string(raw), "model limit") || !strings.Contains(logs.String(), "max_tokens exceeds model limit") {
		t.Fatalf("operator diagnostic: status=%d response=%s logs=%s", status, raw, logs.String())
	}
	auditBody, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	var record auditlog.Record
	if err := json.Unmarshal(auditBody, &record); err != nil || record.ErrorCode != "bridge_upstream_error" || strings.Contains(string(auditBody), "model limit") {
		t.Fatalf("audit attribution: err=%v record=%s", err, auditBody)
	}
}
