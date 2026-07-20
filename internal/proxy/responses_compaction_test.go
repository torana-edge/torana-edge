package proxy

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/provider"
)

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
	srv.proxy.Director(req)
	forwarded, err := io.ReadAll(req.Body)
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
	srv.proxy.Director(req)
	forwarded, err := io.ReadAll(req.Body)
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

func TestApplyOpenAIResponsesCompaction(t *testing.T) {
	chat := &engine.ChatRequest{ProviderExtensions: map[string]any{
		"_openai_variant":      "responses",
		"previous_response_id": "resp_123",
	}}
	applyOpenAIResponsesCompaction(chat, responsesCompactionProvider(75000))

	want := []any{map[string]any{"type": "compaction", "compact_threshold": 75000}}
	if !reflect.DeepEqual(chat.ProviderExtensions["context_management"], want) {
		t.Fatalf("context_management = %#v, want %#v", chat.ProviderExtensions["context_management"], want)
	}
	if chat.ProviderExtensions["previous_response_id"] != "resp_123" {
		t.Fatalf("previous_response_id changed: %#v", chat.ProviderExtensions)
	}
}

func TestApplyOpenAIResponsesCompactionCallerWins(t *testing.T) {
	callerPolicy := []any{map[string]any{"type": "compaction", "compact_threshold": float64(42000)}}
	chat := &engine.ChatRequest{ProviderExtensions: map[string]any{
		"_openai_variant":    "responses",
		"context_management": callerPolicy,
	}}
	applyOpenAIResponsesCompaction(chat, responsesCompactionProvider(75000))
	if !reflect.DeepEqual(chat.ProviderExtensions["context_management"], callerPolicy) {
		t.Fatalf("caller policy was overwritten: %#v", chat.ProviderExtensions["context_management"])
	}
}

func TestApplyOpenAIResponsesCompactionNeverTouchesChatCompletions(t *testing.T) {
	chat := &engine.ChatRequest{ProviderExtensions: map[string]any{
		"previous_response_id": "must-not-imply-responses",
	}}
	applyOpenAIResponsesCompaction(chat, responsesCompactionProvider(75000))
	if _, exists := chat.ProviderExtensions["context_management"]; exists {
		t.Fatalf("Chat Completions request was modified: %#v", chat.ProviderExtensions)
	}
}

func TestApplyOpenAIResponsesCompactionAbsentIsDisabled(t *testing.T) {
	chat := &engine.ChatRequest{ProviderExtensions: map[string]any{"_openai_variant": "responses"}}
	applyOpenAIResponsesCompaction(chat, provider.Provider{Format: "openai"})
	if _, exists := chat.ProviderExtensions["context_management"]; exists {
		t.Fatalf("disabled compaction modified request: %#v", chat.ProviderExtensions)
	}
}
