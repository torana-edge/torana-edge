package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAnthropicToolFailureIsValidIngress(t *testing.T) {
	body := `{"model":"test-model","max_tokens":100,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","is_error":true,"content":"permission denied"}]}]}`
	status, response, hits, _ := parseFailE2E(t, "anthropic", nil, body)
	if status != http.StatusOK || atomic.LoadInt32(hits) != 1 {
		t.Fatalf("valid Anthropic error result rejected: status=%d upstream_hits=%d body=%s", status, atomic.LoadInt32(hits), response)
	}
}

// The same real Anthropic request crosses ingress, host IR, protobuf, a compiled
// official compactor, and upstream serialization. The false row is a positive
// control proving that the guest would otherwise compact this exact content.
func TestOfficialCompactorsPreserveWireToolFailures(t *testing.T) {
	bundles := officialBundlesDir(t)
	for _, name := range []string{"compactor", "keyword_compactor"} {
		t.Run(name, func(t *testing.T) {
			requireBundle(t, bundles, name)
			received := make(chan []byte, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				received <- body
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"r","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer upstream.Close()
			digest, err := plugin.BundleDigestForDir(bundles + "/" + name)
			if err != nil {
				t.Fatal(err)
			}
			approval := provider.PluginApproval{Digest: digest, Permissions: manifestPermissions(bundles + "/" + name), FailureMode: "block"}
			zero, one := 0.0, 1.0
			if name == "compactor" {
				approval.ModelServices = map[string]provider.PluginModelServiceApproval{"summarizer": {Provider: "cheap", Model: "cheap", Path: "/v1/chat/completions", TimeoutMS: 1000, MaxTokens: 512, MaxInputBytes: 1 << 20, MaxCallsPerMinute: 10, MaxTokensPerHour: 10000}}
				approval.PricingResources = map[string]provider.PluginPricingApproval{
					"target":     {Models: []provider.PluginPricingModelApproval{{Provider: "p", Model: "m", CacheReadUSDPerMTok: &one, CacheWriteUSDPerMTok: &one}}},
					"summarizer": {Models: []provider.PluginPricingModelApproval{{InputUSDPerMTok: &zero, OutputUSDPerMTok: &zero, CacheReadUSDPerMTok: &zero, CacheWriteUSDPerMTok: &zero}}},
				}
			}
			srv, err := New(Config{Port: "0", Providers: provider.Config{
				Providers: map[string]provider.Provider{"p": {URL: upstream.URL, Format: "anthropic"}, "cheap": {URL: upstream.URL, Format: "openai", Auth: provider.ProviderAuth{Mode: "none"}}},
				Plugins:   provider.PluginsConfig{Dir: bundles, Order: []string{name}, Approvals: map[string]provider.PluginApproval{name: approval}, Config: map[string]json.RawMessage{name: json.RawMessage(`{"tool_policies":[{"match":"read*","mode":"deterministic","first_pass":true}]}`)}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go srv.Serve(ln)
			defer srv.Shutdown(context.Background())
			text := strings.Repeat("diagnostic evidence without textual failure markers\n", 1000)
			for _, flag := range []bool{true, false} {
				content, _ := json.Marshal(text)
				body := fmt.Sprintf(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"inspect"},{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","is_error":%t,"content":%s}]},{"role":"assistant","content":"reviewed"},{"role":"user","content":"continue"}]}`, flag, content)
				client := http.Client{Timeout: 30 * time.Second}
				resp, err := client.Post("http://"+ln.Addr().String()+"/provider/p/v1/messages", "application/json", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				response, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("status=%d: %s", resp.StatusCode, response)
				}
				var out struct {
					Messages []struct{ Content json.RawMessage }
				}
				select {
				case wire := <-received:
					if err := json.Unmarshal(wire, &out); err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("upstream not called")
				}
				var blocks []struct {
					Content json.RawMessage
					IsError *bool `json:"is_error"`
				}
				if err := json.Unmarshal(out.Messages[2].Content, &blocks); err != nil {
					t.Fatal(err)
				}
				if len(blocks) != 1 || blocks[0].IsError == nil || *blocks[0].IsError != flag {
					t.Fatalf("error flag lost: %+v", blocks)
				}
				var gotText string
				if err := json.Unmarshal(blocks[0].Content, &gotText); err != nil {
					var parts []struct{ Text string }
					if err := json.Unmarshal(blocks[0].Content, &parts); err != nil || len(parts) != 1 {
						t.Fatalf("unexpected tool result content: %s", blocks[0].Content)
					}
					gotText = parts[0].Text
				}
				if flag && gotText != text {
					t.Fatal("explicit failure was compacted")
				}
				if !flag && len(gotText) >= len(text) {
					t.Fatal("positive control did not compact")
				}
			}
		})
	}
}
