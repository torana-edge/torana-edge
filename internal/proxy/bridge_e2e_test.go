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
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/bridge"
	"github.com/torana-edge/torana-edge/internal/provider"
)

var bridgeProtocols = []bridge.Protocol{bridge.OpenAIChat, bridge.OpenAIResponses, bridge.Anthropic, bridge.Gemini, bridge.GeminiCodeAssist}

func bridgeClientBody(p bridge.Protocol, second, stream bool) string {
	var raw string
	switch p {
	case bridge.OpenAIChat:
		history := `{"role":"user","content":"look up weather"}`
		if second {
			history += `,{"role":"assistant","tool_calls":[{"id":"call_123","type":"function","function":{"name":"weather","arguments":"{\"n\":9007199254740993}"}}]},{"role":"tool","tool_call_id":"call_123","content":"sunny"}`
		}
		raw = fmt.Sprintf(`{"model":"client-model","messages":[%s],"max_tokens":128,"stream":%t,"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}]}`, history, stream)
	case bridge.OpenAIResponses:
		history := `{"type":"message","role":"user","content":"look up weather"}`
		if second {
			history += `,{"type":"function_call","call_id":"call_123","name":"weather","arguments":"{\"n\":9007199254740993}"},{"type":"function_call_output","call_id":"call_123","output":"sunny"}`
		}
		raw = fmt.Sprintf(`{"model":"client-model","input":[%s],"max_output_tokens":128,"stream":%t,"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}]}`, history, stream)
	case bridge.Anthropic:
		history := `{"role":"user","content":"look up weather"}`
		if second {
			history += `,{"role":"assistant","content":[{"type":"tool_use","id":"call_123","name":"weather","input":{"n":9007199254740993}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_123","content":"sunny"}]}`
		}
		raw = fmt.Sprintf(`{"model":"client-model","messages":[%s],"max_tokens":128,"stream":%t,"tools":[{"name":"weather","input_schema":{"type":"object"}}]}`, history, stream)
	case bridge.Gemini, bridge.GeminiCodeAssist:
		history := `{"role":"user","parts":[{"text":"look up weather"}]}`
		if second {
			history += `,{"role":"model","parts":[{"functionCall":{"id":"call_123","name":"weather","args":{"n":9007199254740993}}}]},{"role":"user","parts":[{"functionResponse":{"id":"call_123","name":"weather","response":{"content":"sunny"}}}]}`
		}
		raw = fmt.Sprintf(`{"contents":[%s],"generationConfig":{"maxOutputTokens":128},"tools":[{"functionDeclarations":[{"name":"weather","parameters":{"type":"object"}}]}]}`, history)
		if p == bridge.GeminiCodeAssist {
			raw = `{"model":"client-model","project":"client-project","request":` + raw + `}`
		}
	}
	return raw
}

func bridgeUpstreamJSON(p bridge.Protocol, tool bool) string {
	switch p {
	case bridge.OpenAIChat:
		msg, finish := `{"role":"assistant","content":"It is sunny"}`, "stop"
		if tool {
			msg = `{"role":"assistant","content":null,"tool_calls":[{"id":"call_123","type":"function","function":{"name":"weather","arguments":"{\"n\":9007199254740993}"}}]}`
			finish = "tool_calls"
		}
		return fmt.Sprintf(`{"id":"reply_123","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":%s,"finish_reason":%q}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`, msg, finish)
	case bridge.OpenAIResponses:
		output := `{"type":"message","id":"message_123","status":"completed","role":"assistant","content":[{"type":"output_text","text":"It is sunny","annotations":[]}]}`
		if tool {
			output = `{"type":"function_call","id":"item_123","call_id":"call_123","name":"weather","arguments":"{\"n\":9007199254740993}","status":"completed"}`
		}
		return fmt.Sprintf(`{"id":"reply_123","object":"response","status":"completed","model":"upstream-model","output":[%s],"usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13}}`, output)
	case bridge.Anthropic:
		content, finish := `{"type":"text","text":"It is sunny"}`, "end_turn"
		if tool {
			content = `{"type":"tool_use","id":"call_123","name":"weather","input":{"n":9007199254740993}}`
			finish = "tool_use"
		}
		return fmt.Sprintf(`{"id":"reply_123","type":"message","model":"upstream-model","role":"assistant","content":[%s],"stop_reason":%q,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":3}}`, content, finish)
	default:
		part := `{"text":"It is sunny"}`
		if tool {
			part = `{"functionCall":{"id":"call_123","name":"weather","args":{"n":9007199254740993}}}`
		}
		raw := fmt.Sprintf(`{"responseId":"reply_123","modelVersion":"upstream-model","candidates":[{"index":0,"content":{"role":"model","parts":[%s]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":13}}`, part)
		if p == bridge.GeminiCodeAssist {
			return `{"response":` + raw + `}`
		}
		return raw
	}
}

func newBridgeProxy(t *testing.T, client, upstream bridge.Protocol, handler http.HandlerFunc) (*httptest.Server, *Server) {
	t.Helper()
	target := httptest.NewServer(handler)
	t.Cleanup(target.Close)
	cfg := provider.Provider{URL: target.URL, Format: upstream.Format(), Auth: provider.ProviderAuth{Mode: "none"}, Bridge: &provider.BridgeConfig{Client: client, Upstream: upstream, Model: "upstream-model"}}
	if upstream == bridge.GeminiCodeAssist {
		cfg.Bridge.Project = "upstream-project"
	}
	srv, err := New(Config{Providers: provider.Config{Providers: map[string]provider.Provider{"p": cfg}}})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { proxy.Close(); _ = srv.Shutdown(context.Background()) })
	return proxy, srv
}

func callBridge(t *testing.T, proxy *httptest.Server, p bridge.Protocol, body string, stream bool) (int, []byte, http.Header) {
	t.Helper()
	path, _, err := bridge.Endpoint(p, "client-model", stream)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/provider/p"+path+"?key=client-secret&client_option=1", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("X-Api-Key", "client-secret")
	req.Header.Set("Anthropic-Beta", "client-only-beta")
	req.Header.Set("X-Provider-Private", "client-private")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v; %s", err, raw)
	}
	return resp.StatusCode, raw, resp.Header
}

func TestBridgeJSONToolLoopMatrix(t *testing.T) {
	for _, client := range bridgeProtocols {
		for _, upstream := range bridgeProtocols {
			if client == upstream {
				continue
			}
			t.Run(string(client)+"_to_"+string(upstream), func(t *testing.T) {
				var calls atomic.Int32
				proxy, _ := newBridgeProxy(t, client, upstream, func(w http.ResponseWriter, r *http.Request) {
					n := calls.Add(1)
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						http.Error(w, "read failed", 500)
						return
					}
					wantPath, wantQuery, _ := bridge.Endpoint(upstream, "upstream-model", false)
					if r.URL.Path != wantPath || r.URL.RawQuery != wantQuery {
						t.Errorf("wrong upstream URL: %s", r.URL.String())
					}
					if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Provider-Private") != "" || r.Header.Get("Anthropic-Beta") != "" {
						t.Errorf("client headers leaked: %v", r.Header)
					}
					if upstream == bridge.Anthropic && r.Header.Get("Anthropic-Version") != "2023-06-01" {
						t.Error("missing Anthropic version")
					}
					chat, err := bridge.ParseRequest(upstream, raw, r.URL.Path)
					if err != nil {
						t.Errorf("upstream rejects request: %v; %s", err, raw)
					} else {
						if chat.Model != "upstream-model" {
							t.Errorf("wrong model %s", chat.Model)
						}
						if chat.MaxTokens == nil || *chat.MaxTokens != 128 {
							t.Errorf("wrong token limit: %s", raw)
						}
						if n == 2 {
							results := 0
							for _, msg := range chat.Messages {
								for _, b := range msg.Blocks {
									if b.ToolResult != nil {
										results++
										if b.ToolResult.ToolCallID != "call_123" {
											t.Error("tool result ID changed")
										}
									}
								}
							}
							if results != 1 {
								t.Errorf("lost tool result: %s", raw)
							}
						}
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, bridgeUpstreamJSON(upstream, n == 1))
				})
				for turn := 0; turn < 2; turn++ {
					status, raw, _ := callBridge(t, proxy, client, bridgeClientBody(client, turn == 1, false), false)
					if status != 200 {
						t.Fatalf("turn %d status %d: %s", turn, status, raw)
					}
					// Check the client envelope as well as the shared content facts.
					var obj map[string]json.RawMessage
					if json.Unmarshal(raw, &obj) != nil {
						t.Fatalf("invalid client JSON: %s", raw)
					}
					if turn == 0 {
						if !bytes.Contains(raw, []byte("call_123")) || !bytes.Contains(raw, []byte("9007199254740993")) {
							t.Fatalf("lost tool identity or arguments: %s", raw)
						}
					} else if !bytes.Contains(raw, []byte("It is sunny")) {
						t.Fatalf("lost final text: %s", raw)
					}
					switch client {
					case bridge.Anthropic:
						if _, ok := obj["content"]; !ok {
							t.Fatalf("wrong client contract %s", raw)
						}
					case bridge.OpenAIChat:
						if _, ok := obj["choices"]; !ok {
							t.Fatalf("wrong client contract %s", raw)
						}
					case bridge.OpenAIResponses:
						if _, ok := obj["output"]; !ok {
							t.Fatalf("wrong client contract %s", raw)
						}
					}
				}
				if calls.Load() != 2 {
					t.Fatalf("upstream calls=%d", calls.Load())
				}
			})
		}
	}
}

func TestBridgeRejectsBeforeUpstreamWithoutPlugins(t *testing.T) {
	var calls atomic.Int32
	proxy, _ := newBridgeProxy(t, bridge.OpenAIResponses, bridge.Anthropic, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) })
	status, raw, _ := callBridge(t, proxy, bridge.OpenAIResponses, `{"input":"hello","max_output_tokens":8,"previous_response_id":"do-not-send"}`, false)
	if status != 400 || calls.Load() != 0 || bytes.Contains(raw, []byte("do-not-send")) {
		t.Fatalf("status=%d calls=%d body=%s", status, calls.Load(), raw)
	}
	if !bytes.Contains(raw, []byte("full conversation")) {
		t.Fatalf("missing actionable capability explanation: %s", raw)
	}
}

func TestBridgeMapsUpstreamErrors(t *testing.T) {
	for _, client := range bridgeProtocols {
		t.Run(string(client), func(t *testing.T) {
			proxy, _ := newBridgeProxy(t, client, bridge.OpenAIChat, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "4")
				w.WriteHeader(429)
				_, _ = io.WriteString(w, "<html>upstream-private</html>")
			})
			status, raw, headers := callBridge(t, proxy, client, bridgeClientBody(client, false, false), false)
			if status != 429 || headers.Get("Retry-After") != "4" || !json.Valid(raw) || bytes.Contains(raw, []byte("upstream-private")) {
				t.Fatalf("status=%d headers=%v body=%s", status, headers, raw)
			}
		})
	}
}
