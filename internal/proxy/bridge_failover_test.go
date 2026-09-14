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

	"github.com/torana-edge/torana-edge/internal/bridge"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestBridgeFallbackChangesProtocolAndModel(t *testing.T) {
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("primary path: %s", r.URL.Path)
		}
		w.WriteHeader(503)
	}))
	t.Cleanup(primary.Close)
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/messages" || r.Header.Get("Anthropic-Version") != "2023-06-01" {
			t.Errorf("backup transport: %s %v", r.URL.Path, r.Header)
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("credential leaked during retry")
		}
		chat, err := bridge.ParseRequest(bridge.Anthropic, raw, r.URL.Path)
		if err != nil || chat.Model != "backup-model" {
			t.Errorf("backup request: %s; %v", raw, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.ReplaceAll(bridgeUpstreamJSON(bridge.Anthropic, false), "upstream-model", "backup-model"))
	}))
	t.Cleanup(backup.Close)
	cfg := provider.Config{Providers: map[string]provider.Provider{
		"p":      {URL: primary.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}, Fallback: []string{"backup"}, Bridge: &provider.BridgeConfig{Client: bridge.OpenAIResponses, Upstream: bridge.OpenAIChat, Model: "primary-model"}},
		"backup": {URL: backup.URL, Format: "anthropic", Auth: provider.ProviderAuth{Mode: "none"}, Bridge: &provider.BridgeConfig{Client: bridge.OpenAIResponses, Upstream: bridge.Anthropic, Model: "backup-model", MaxTokens: 256}},
	}}
	srv, err := New(Config{Providers: cfg})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { proxy.Close(); _ = srv.Shutdown(context.Background()) })
	status, raw, _ := callBridge(t, proxy, bridge.OpenAIResponses, bridgeClientBody(bridge.OpenAIResponses, false, false), false)
	if status != 200 || !strings.Contains(string(raw), `"output"`) || !strings.Contains(string(raw), "backup-model") {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if primaryCalls.Load() != 1 || backupCalls.Load() != 1 {
		t.Fatalf("attempts primary=%d backup=%d", primaryCalls.Load(), backupCalls.Load())
	}
}

func TestBridgeFallbackSkipsUnrepresentableRequest(t *testing.T) {
	var backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	t.Cleanup(primary.Close)
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { backupCalls.Add(1); w.WriteHeader(200) }))
	t.Cleanup(backup.Close)
	cfg := provider.Config{Providers: map[string]provider.Provider{
		"p":      {URL: primary.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}, Fallback: []string{"backup"}, Bridge: &provider.BridgeConfig{Client: bridge.OpenAIResponses, Upstream: bridge.OpenAIChat}},
		"backup": {URL: backup.URL, Format: "anthropic", Auth: provider.ProviderAuth{Mode: "none"}, Bridge: &provider.BridgeConfig{Client: bridge.OpenAIResponses, Upstream: bridge.Anthropic, MaxTokens: 256}},
	}}
	srv, err := New(Config{Providers: cfg})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { proxy.Close(); _ = srv.Shutdown(context.Background()) })
	body := `{"input":"hello","tools":[{"type":"function","name":"t","parameters":{"type":"object"},"strict":true}],"max_output_tokens":32}`
	status, _, _ := callBridge(t, proxy, bridge.OpenAIResponses, body, false)
	if status != 503 || backupCalls.Load() != 0 {
		t.Fatalf("status=%d backup calls=%d", status, backupCalls.Load())
	}
}

func TestBridgeFallbackProjectsAcceptedClientRequestAndRefreshesAttemptFacts(t *testing.T) {
	var fallbackRaw []byte
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		if json.Unmarshal(raw, &body) != nil || body["context_management"] == nil {
			t.Errorf("primary request did not receive Responses compaction: %s", raw)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(primary.Close)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackRaw, _ = io.ReadAll(r.Body)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("fallback path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, bridgeUpstreamJSON(bridge.Anthropic, false))
	}))
	t.Cleanup(fallback.Close)
	cfg := provider.Config{Providers: map[string]provider.Provider{
		"p": {
			URL: primary.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"},
			Fallback:            []string{"backup"},
			Bridge:              &provider.BridgeConfig{Client: bridge.OpenAIResponses, Upstream: bridge.OpenAIResponses, Model: "primary-model"},
			ResponsesCompaction: &provider.ResponsesCompactionConfig{CompactThreshold: 4096},
		},
		"backup": {
			URL: fallback.URL, Format: "anthropic", Auth: provider.ProviderAuth{Mode: "none"},
			Bridge: &provider.BridgeConfig{Client: bridge.OpenAIResponses, Upstream: bridge.Anthropic, Model: "backup-model", MaxTokens: 256},
		},
	}}
	srv, err := New(Config{Providers: cfg})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { proxy.Close(); _ = srv.Shutdown(context.Background()) })

	status, raw, _ := callBridge(t, proxy, bridge.OpenAIResponses,
		`{"instructions":"keep this stable","input":"hello","max_output_tokens":32}`, false)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if len(fallbackRaw) == 0 {
		t.Fatal("fallback was not called")
	}
	fallbackChat, err := bridge.ParseRequest(bridge.Anthropic, fallbackRaw, "/v1/messages")
	if err != nil {
		t.Fatalf("parse fallback request: %v; %s", err, fallbackRaw)
	}
	if !fallbackChat.ProviderExtensions.IsAbsent() {
		t.Fatalf("primary Responses extensions reached Anthropic fallback: %s", fallbackRaw)
	}

	records := srv.conversations.List()
	if len(records) != 1 {
		t.Fatalf("conversation records=%d, want 1", len(records))
	}
	got := records[0]
	if got.Provider != "backup" || got.Model != "backup-model" {
		t.Errorf("provider/model=%q/%q", got.Provider, got.Model)
	}
	if got.Format != "openai" || got.Path != "/v1/responses" {
		t.Errorf("client format/path=%q/%q, want openai//v1/responses", got.Format, got.Path)
	}
	if want := requestCachePrefixKey(fallbackChat); got.CachePrefixKey != want {
		t.Errorf("cache key=%q, want fallback key %q", got.CachePrefixKey, want)
	}
}

func TestBridgeFallbackRequiresMatchingClientContract(t *testing.T) {
	var backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(primary.Close)
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backup.Close)
	cfg := provider.Config{Providers: map[string]provider.Provider{
		"p":      {URL: primary.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}, Fallback: []string{"backup"}, Bridge: &provider.BridgeConfig{Client: bridge.OpenAIResponses, Upstream: bridge.OpenAIChat}},
		"backup": {URL: backup.URL, Format: "anthropic", Auth: provider.ProviderAuth{Mode: "none"}, Bridge: &provider.BridgeConfig{Client: bridge.Anthropic, Upstream: bridge.Anthropic}},
	}}
	srv, err := New(Config{Providers: cfg})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { proxy.Close(); _ = srv.Shutdown(context.Background()) })
	status, _, _ := callBridge(t, proxy, bridge.OpenAIResponses, `{"input":"hello","max_output_tokens":32}`, false)
	if status != http.StatusServiceUnavailable || backupCalls.Load() != 0 {
		t.Fatalf("status=%d backup calls=%d", status, backupCalls.Load())
	}
}

func TestBridgeFallbackRecordsActualAttemptProtocolPathAndCacheKey(t *testing.T) {
	accepted, err := bridge.ParseRequest(bridge.OpenAIResponses,
		[]byte(`{"model":"client-model","instructions":"stable","input":"hello","max_output_tokens":32}`),
		"/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	primaryChat, err := bridge.ProjectRequest(accepted, bridge.OpenAIResponses, bridge.OpenAIChat, bridge.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	primaryRaw, err := bridge.MarshalRequest(bridge.OpenAIChat, primaryChat)
	if err != nil {
		t.Fatal(err)
	}

	state := &reqState{}
	exchange := &bridgeExchange{Client: bridge.OpenAIResponses, Upstream: bridge.OpenAIChat, ClientRequest: accepted, AcceptedRequest: accepted}
	rc := &RouteContext{ProviderName: "p", StrippedPath: "/v1/responses"}
	ctx := context.WithValue(context.Background(), reqStateKey{}, state)
	ctx = context.WithValue(ctx, routeContextKey{}, rc)
	ctx = context.WithValue(ctx, bridgeContextKey{}, exchange)
	ctx = context.WithValue(ctx, engine.ChatRequestKey, primaryChat)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://primary.example/v1/chat/completions", strings.NewReader(string(primaryRaw)))
	if err != nil {
		t.Fatal(err)
	}

	cfg := provider.Config{Providers: map[string]provider.Provider{
		"p":      {URL: "https://primary.example", Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}, Fallback: []string{"backup"}, Bridge: &provider.BridgeConfig{Client: bridge.OpenAIResponses, Upstream: bridge.OpenAIChat}},
		"backup": {URL: "https://backup.example", Format: "anthropic", Auth: provider.ProviderAuth{Mode: "none"}, Bridge: &provider.BridgeConfig{Client: bridge.OpenAIResponses, Upstream: bridge.Anthropic, Model: "backup-model", MaxTokens: 256}},
	}}
	var calls int
	var fallbackChat *engine.ChatRequest
	limiter := NewRateLimiter(0, 1)
	t.Cleanup(limiter.Close)
	transport := &failoverRoundTripper{
		cfg:         func() provider.Config { return cfg },
		rateLimiter: limiter,
		base: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Header: make(http.Header), Request: r}, nil
			}
			raw, _ := io.ReadAll(r.Body)
			fallbackChat, err = bridge.ParseRequest(bridge.Anthropic, raw, r.URL.Path)
			if err != nil {
				t.Fatalf("parse fallback request: %v; %s", err, raw)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header), Request: r}, nil
		}),
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if calls != 2 {
		t.Fatalf("attempts=%d, want 2", calls)
	}
	if state.ActualFormat != "anthropic" || state.ActualPath != "/v1/messages" {
		t.Errorf("actual format/path=%q/%q", state.ActualFormat, state.ActualPath)
	}
	if want := requestCachePrefixKey(fallbackChat); state.CachePrefixKey != want {
		t.Errorf("cache key=%q, want fallback key %q", state.CachePrefixKey, want)
	}
}

func TestBridgeFallbackAppliesTargetResponsesCompaction(t *testing.T) {
	accepted, err := bridge.ParseRequest(bridge.OpenAIResponses,
		[]byte(`{"model":"client-model","input":"hello","max_output_tokens":32}`),
		"/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	exchange := &bridgeExchange{
		Client: bridge.OpenAIResponses, Upstream: bridge.Anthropic,
		ClientRequest: accepted, AcceptedRequest: accepted,
	}
	ctx := context.WithValue(context.Background(), bridgeContextKey{}, exchange)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://primary.example/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	target := provider.Provider{
		URL: "https://fallback.example", Format: "openai", Auth: provider.ProviderAuth{Mode: "none"},
		Bridge:              &provider.BridgeConfig{Client: bridge.OpenAIResponses, Upstream: bridge.OpenAIResponses, Model: "fallback-model"},
		ResponsesCompaction: &provider.ResponsesCompactionConfig{CompactThreshold: 8192},
	}
	bridged, err := prepareBridgeRetry(req, target)
	if err != nil || !bridged {
		t.Fatalf("prepare retry: bridged=%t err=%v", bridged, err)
	}
	raw, _ := io.ReadAll(req.Body)
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body["context_management"] == nil {
		t.Fatalf("fallback Responses compaction missing: %s", raw)
	}
}

func TestBridgePluginRouteChangesUpstreamContract(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-router/plugin.wasm")
	var initialCalls, routedCalls atomic.Int32
	initial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { initialCalls.Add(1); w.WriteHeader(500) }))
	t.Cleanup(initial.Close)
	routed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routedCalls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		chat, err := bridge.ParseRequest(bridge.Anthropic, raw, r.URL.Path)
		if err != nil || chat.Model != "small-model" {
			t.Errorf("routed request %s; %v", raw, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, bridgeUpstreamJSON(bridge.Anthropic, false))
	}))
	t.Cleanup(routed.Close)
	cfg := provider.Config{Providers: map[string]provider.Provider{
		"p":     {URL: initial.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}, Bridge: &provider.BridgeConfig{Client: bridge.OpenAIResponses, Upstream: bridge.OpenAIChat}},
		"cheap": {URL: routed.URL, Format: "anthropic", Auth: provider.ProviderAuth{Mode: "none"}, Bridge: &provider.BridgeConfig{Client: bridge.OpenAIResponses, Upstream: bridge.Anthropic, MaxTokens: 256}},
	}, Plugins: provider.PluginsConfig{Dir: "../../examples/plugins", Order: []string{"test-router"}, AllowUnapproved: true}}
	srv, err := New(Config{Providers: cfg})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { proxy.Close(); _ = srv.Shutdown(context.Background()) })
	status, raw, _ := callBridge(t, proxy, bridge.OpenAIResponses, `{"input":"routecheap please","max_output_tokens":32}`, false)
	if status != 200 || !strings.Contains(string(raw), `"output"`) {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if initialCalls.Load() != 0 || routedCalls.Load() != 1 {
		t.Fatalf("attempts initial=%d routed=%d", initialCalls.Load(), routedCalls.Load())
	}
}
