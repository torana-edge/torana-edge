package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"reflect"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/provider"
)

// runRewrite drives the proxy's Rewrite hook the way httputil.ReverseProxy does:
// clone the inbound request, hand the clone over as ProxyRequest.Out, and return
// the mutated clone. Tests assert on what would go upstream, not on the inbound
// request, which Rewrite is contractually forbidden from modifying.
func runRewrite(t *testing.T, srv *Server, req *http.Request) *http.Request {
	t.Helper()
	pr := &httputil.ProxyRequest{In: req, Out: req.Clone(req.Context())}
	srv.proxy.Rewrite(pr)
	return pr.Out
}

func responsesCompactionProvider(threshold int) provider.Provider {
	return provider.Provider{
		Format: "openai",
		ResponsesCompaction: &provider.ResponsesCompactionConfig{
			CompactThreshold: threshold,
		},
	}
}

func TestResponsesCompactionDirectorInjectionAndOpaqueHistory(t *testing.T) {
	providers := testProviderConfig("https://api.openai.com", "openai", "openai")
	p := providers.Providers["openai"]
	p.ResponsesCompaction = &provider.ResponsesCompactionConfig{CompactThreshold: 75000}
	providers.Providers["openai"] = p
	srv, err := New(Config{Port: "0", Providers: providers})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"model":"gpt-5.4","stream":true,"previous_response_id":"resp_123","input":[{"type":"reasoning","encrypted_content":"reasoning-opaque"},{"type":"message","role":"user","content":"next"},{"type":"compaction","encrypted_content":"compaction-opaque"}]}`
	req := httptest.NewRequest("POST", "http://torana/provider/openai/v1/responses", strings.NewReader(body))
	out := runRewrite(t, srv, req)
	forwarded, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(forwarded, &got); err != nil {
		t.Fatalf("forwarded request is invalid JSON: %v: %s", err, forwarded)
	}
	if got["previous_response_id"] != "resp_123" {
		t.Fatalf("previous_response_id changed: %s", forwarded)
	}
	wantPolicy := []any{map[string]any{"type": "compaction", "compact_threshold": float64(75000)}}
	if !reflect.DeepEqual(got["context_management"], wantPolicy) {
		t.Fatalf("context_management = %#v, want %#v", got["context_management"], wantPolicy)
	}
	if _, exists := got["stream_options"]; exists {
		t.Fatalf("Responses request received Chat Completions stream_options: %s", forwarded)
	}
	items := got["input"].([]any)
	if items[0].(map[string]any)["encrypted_content"] != "reasoning-opaque" || items[2].(map[string]any)["encrypted_content"] != "compaction-opaque" {
		t.Fatalf("opaque history changed or moved: %s", forwarded)
	}
}

func TestResponsesCompactionDirectorNeverInjectsChatCompletions(t *testing.T) {
	providers := testProviderConfig("https://api.openai.com", "openai", "openai")
	p := providers.Providers["openai"]
	p.ResponsesCompaction = &provider.ResponsesCompactionConfig{CompactThreshold: 75000}
	providers.Providers["openai"] = p
	srv, err := New(Config{Port: "0", Providers: providers})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "http://torana/provider/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	out := runRewrite(t, srv, req)
	forwarded, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(forwarded, &got); err != nil {
		t.Fatal(err)
	}
	if _, exists := got["context_management"]; exists {
		t.Fatalf("Chat Completions request was modified: %s", forwarded)
	}
	streamOptions, ok := got["stream_options"].(map[string]any)
	if !ok || streamOptions["include_usage"] != true {
		t.Fatalf("streamed Chat Completions must retain usage injection: %s", forwarded)
	}
}

func mustExts(m map[string]any) engine.OptionalJSONObject {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	r, err := engine.ParseOptionalJSONObject(b)
	if err != nil {
		panic(err)
	}
	return r
}

func TestApplyOpenAIResponsesCompaction(t *testing.T) {
	chat := &engine.ChatRequest{ProviderExtensions: mustExts(map[string]any{
		"_openai_variant":      "responses",
		"previous_response_id": "resp_123",
	})}
	applyOpenAIResponsesCompaction(chat, responsesCompactionProvider(75000))

	want := []any{map[string]any{"type": "compaction", "compact_threshold": float64(75000)}}
	got, _, err := chat.ProviderExtensions.DecodeObject()
	if err != nil {
		t.Fatalf("decode extensions: %v", err)
	}
	var gotCM []any
	if err := json.Unmarshal(got["context_management"], &gotCM); err != nil {
		t.Fatalf("context_management not a JSON array: %v", err)
	}
	if !reflect.DeepEqual(gotCM, want) {
		t.Fatalf("context_management = %#v, want %#v", gotCM, want)
	}
	if string(got["previous_response_id"]) != `"resp_123"` {
		t.Fatalf("previous_response_id changed: %#v", chat.ProviderExtensions)
	}
}

func TestApplyOpenAIResponsesCompactionCallerWins(t *testing.T) {
	callerPolicy := []any{map[string]any{"type": "compaction", "compact_threshold": float64(42000)}}
	chat := &engine.ChatRequest{ProviderExtensions: mustExts(map[string]any{
		"_openai_variant":    "responses",
		"context_management": callerPolicy,
	})}
	applyOpenAIResponsesCompaction(chat, responsesCompactionProvider(75000))
	got, _, err := chat.ProviderExtensions.DecodeObject()
	if err != nil {
		t.Fatalf("decode extensions: %v", err)
	}
	var gotCM []any
	if err := json.Unmarshal(got["context_management"], &gotCM); err != nil {
		t.Fatalf("context_management not a JSON array: %v", err)
	}
	if !reflect.DeepEqual(gotCM, callerPolicy) {
		t.Fatalf("caller policy was overwritten: %#v", gotCM)
	}
}

func TestApplyOpenAIResponsesCompactionNeverTouchesChatCompletions(t *testing.T) {
	chat := &engine.ChatRequest{ProviderExtensions: mustExts(map[string]any{
		"previous_response_id": "must-not-imply-responses",
	})}
	applyOpenAIResponsesCompaction(chat, responsesCompactionProvider(75000))
	cm, _, err := chat.ProviderExtensions.DecodeObject()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, exists := cm["context_management"]; exists {
		t.Fatalf("Chat Completions request was modified: %#v", cm)
	}
}

func TestApplyOpenAIResponsesCompactionAbsentIsDisabled(t *testing.T) {
	chat := &engine.ChatRequest{ProviderExtensions: mustExts(map[string]any{"_openai_variant": "responses"})}
	applyOpenAIResponsesCompaction(chat, provider.Provider{Format: "openai"})
	cm, _, err := chat.ProviderExtensions.DecodeObject()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, exists := cm["context_management"]; exists {
		t.Fatalf("disabled compaction modified request: %#v", cm)
	}
}
