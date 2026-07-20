package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// TestPromptCacheKeyPassthrough: OpenAI's caching is automatic, but the
// optional prompt_cache_key routing hint must survive the IR round-trip
// (unparsed top-level field → ProviderExtensions).
func TestPromptCacheKeyPassthrough(t *testing.T) {
	adapter := &Adapter{}
	input := `{
		"model": "gpt-4o",
		"prompt_cache_key": "session-42",
		"messages": [{"role": "user", "content": "hi"}]
	}`
	chat, err := adapter.Unmarshal([]byte(input))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := adapter.Marshal(chat)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["prompt_cache_key"] != "session-42" {
		t.Errorf("prompt_cache_key dropped: %s", out)
	}
}

// TestStreamUsageCachedTokens: usage.prompt_tokens_details.cached_tokens on
// the final chunk flows into the canonical usage event and is re-emitted by
// the serializer.
func TestStreamUsageCachedTokens(t *testing.T) {
	sse := `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":2000,"completion_tokens":10,"total_tokens":2010,"prompt_tokens_details":{"cached_tokens":1792}}}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	sa := &StreamAdapter{}
	var usage *engine.StreamUsage
	for ev := range sa.ParseStream(strings.NewReader(sse)) {
		if ev.Usage != nil {
			usage = ev.Usage
		}
	}
	if usage == nil {
		t.Fatal("no usage event emitted")
	}
	if usage.InputTokens != 2000 || usage.OutputTokens != 10 || usage.CacheReadTokens != 1792 {
		t.Errorf("usage = %+v, want 2000/10 with 1792 cached", usage)
	}

	out := usageSSE(usage)
	if !strings.Contains(out, `"cached_tokens":1792`) {
		t.Errorf("serialized usage chunk missing cached_tokens: %s", out)
	}
	// Uncached usage keeps the classic shape.
	if out := usageSSE(&engine.StreamUsage{InputTokens: 1, OutputTokens: 2}); strings.Contains(out, "prompt_tokens_details") {
		t.Errorf("uncached usage should omit prompt_tokens_details: %s", out)
	}
}

func TestStreamUsageDeepSeekCacheTokens(t *testing.T) {
	input := `data: {"choices":[],"usage":{"prompt_tokens":2000,"completion_tokens":10,"prompt_cache_hit_tokens":1536,"prompt_cache_miss_tokens":464}}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	var usage *engine.StreamUsage
	for event := range (&StreamAdapter{}).ParseStream(strings.NewReader(input)) {
		if event.Usage != nil {
			usage = event.Usage
		}
	}
	if usage == nil || usage.InputTokens != 2000 || usage.OutputTokens != 10 || usage.CacheReadTokens != 1536 {
		t.Fatalf("DeepSeek usage not parsed: %+v", usage)
	}
}
