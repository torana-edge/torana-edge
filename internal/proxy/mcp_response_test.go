package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/torana-edge/torana-edge/internal/bridge"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestMCPObservationThroughProxyAllShapes(t *testing.T) {
	for _, shape := range bridgeProtocols {
		for _, upstreamShape := range bridgeProtocols {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s_from_%s/stream=%t", shape, upstreamShape, stream), func(t *testing.T) {
					wire := bridgeUpstreamJSON(upstreamShape, true)
					if stream {
						wire = bridgeUpstreamSSE(upstreamShape, true) + "\n\n"
					}
					wire = strings.ReplaceAll(wire, "weather", "mcp__torana__torana_invoke")
					requests := make(chan []byte, 2)
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						raw, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						requests <- raw
						contentType := "application/json"
						if stream {
							contentType = "text/event-stream"
						}
						w.Header().Set("Content-Type", contentType)
						fmt.Fprint(w, wire)
					}))
					defer upstream.Close()
					s := newMCPTestServer(t)
					cfg := s.GetConfig().Providers
					p := provider.Provider{URL: upstream.URL, Format: upstreamShape.Format(), Auth: provider.ProviderAuth{Mode: "none"}}
					if shape != upstreamShape {
						p.Bridge = &provider.BridgeConfig{Client: shape, Upstream: upstreamShape, Model: "upstream-model"}
						if upstreamShape == bridge.GeminiCodeAssist {
							p.Bridge.Project = "upstream-project"
						}
					}
					cfg.Providers = map[string]provider.Provider{"p": p}
					if err := s.SetProviders(cfg); err != nil {
						t.Fatal(err)
					}
					proxy := httptest.NewServer(s.Handler())
					defer proxy.Close()
					body := strings.ReplaceAll(bridgeClientBody(shape, false, stream), "weather", "mcp__torana__torana_invoke")
					status, baseline, _ := callBridge(t, proxy, shape, body, stream)
					if status != 200 {
						t.Fatalf("baseline status=%d", status)
					}
					if _, ok := s.mcpCorrelation.Consume("torana_invoke", json.RawMessage(`{"n":9007199254740993}`), time.Now()); ok {
						t.Fatal("disabled MCP recorded evidence")
					}
					cfg.MCP.Enabled = true
					if err := s.SetProviders(cfg); err != nil {
						t.Fatal(err)
					}
					status, observed, _ := callBridge(t, proxy, shape, body, stream)
					if status != 200 || !bytes.Equal(baseline, observed) {
						t.Fatal("observation changed client response bytes")
					}
					if !bytes.Equal(<-requests, <-requests) {
						t.Fatal("observation changed provider request bytes/prompt cache prefix")
					}
					binding, ok := s.mcpCorrelation.Consume("torana_invoke", json.RawMessage(`{"n":9007199254740993}`), time.Now())
					if !ok || binding.ConversationID == "" || binding.CallID == "" {
						t.Fatalf("proxy did not record complete tool call: %+v ok=%v", binding, ok)
					}
					if _, ok := s.mcpCorrelation.Consume("torana_invoke", json.RawMessage(`{"n":9007199254740993}`), time.Now()); ok {
						t.Fatal("observation replay granted a second binding")
					}
				})
			}
		}
	}
}

func TestMCPObservationClientCancellationUnblocksRealParser(t *testing.T) {
	upstreamDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+`{"id":"response","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call","type":"function","function":{"name":"mcp__torana__torana_invoke","arguments":"{\"n\":1}"}}]}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	s := newMCPTestServer(t)
	cfg := s.GetConfig().Providers
	cfg.MCP.Enabled = true
	cfg.Providers = map[string]provider.Provider{"p": {URL: upstream.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}}}
	if err := s.SetProviders(cfg); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(s.Handler())
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, "POST", proxy.URL+"/provider/p/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resp.Body.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-upstreamDone:
	case <-time.After(time.Second):
		t.Fatal("MCP observation left upstream parser blocked")
	}
	if _, ok := s.mcpCorrelation.Consume("torana_invoke", json.RawMessage(`{"n":1}`), time.Now()); ok {
		t.Fatal("cancelled stream granted a binding")
	}
}

func TestObservedProviderCallBindsActualMCPInvocation(t *testing.T) {
	wire := strings.ReplaceAll(bridgeUpstreamJSON(bridge.Anthropic, true), "weather", "mcp__torana__torana_invoke")
	wire = strings.ReplaceAll(wire, `"input":{"n":9007199254740993}`, `"input":{"namespace":"torana","operation":"system.status"}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, wire)
	}))
	defer upstream.Close()
	s := newMCPTestServer(t)
	cfg := s.GetConfig().Providers
	cfg.MCP.Enabled = true
	cfg.Providers = map[string]provider.Provider{"p": {URL: upstream.URL, Format: "anthropic", Auth: provider.ProviderAuth{Mode: "none"}}}
	if err := s.SetProviders(cfg); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(s.Handler())
	defer proxy.Close()
	status, _, _ := callBridge(t, proxy, bridge.Anthropic, bridgeClientBody(bridge.Anthropic, false, false), false)
	if status != 200 {
		t.Fatal("provider call failed")
	}
	token, err := s.mcpTokens.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "binding-test", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: proxy.URL + "/_torana/mcp", HTTPClient: &http.Client{Transport: mcpTestTransport{token: token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, expected := range []string{"bound", "unbound"} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "torana_invoke", Arguments: map[string]any{"namespace": "torana", "operation": "system.status"}})
		if err != nil || result.IsError {
			t.Fatal("observed MCP invocation failed")
		}
		encoded, err := json.Marshal(result.StructuredContent)
		var envelope struct{ Conversation struct{ Binding string } }
		if err != nil || json.Unmarshal(encoded, &envelope) != nil || envelope.Conversation.Binding != expected {
			t.Fatalf("MCP binding result did not match %s", expected)
		}
	}
}
