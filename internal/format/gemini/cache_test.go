package gemini

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// TestCachedContentPassthrough: an explicit cachedContent reference (Gemini's
// opt-in context cache) is an unparsed top-level field and must survive the
// IR round-trip via ProviderExtensions — for both the bare Gemini shape and
// the Code Assist wrapper.
func TestCachedContentPassthrough(t *testing.T) {
	adapter := &Adapter{}

	t.Run("bare", func(t *testing.T) {
		input := `{
			"cachedContent": "cachedContents/abc123",
			"contents": [{"role": "user", "parts": [{"text": "hi"}]}]
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
		if m["cachedContent"] != "cachedContents/abc123" {
			t.Errorf("cachedContent dropped: %s", out)
		}
	})

	t.Run("codeassist", func(t *testing.T) {
		input := `{
			"model": "gemini-3-pro",
			"project": "p",
			"request": {
				"cachedContent": "cachedContents/abc123",
				"contents": [{"role": "user", "parts": [{"text": "hi"}]}]
			}
		}`
		chat, err := adapter.Unmarshal([]byte(input))
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		out, err := adapter.Marshal(chat)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m struct {
			Request map[string]any `json:"request"`
		}
		json.Unmarshal(out, &m)
		if m.Request["cachedContent"] != "cachedContents/abc123" {
			t.Errorf("wrapped cachedContent dropped: %s", out)
		}
	})
}

// TestStreamCachedContentTokenCount: the real Code Assist capture reports
// 24430 cached prompt tokens — they must land on the canonical usage event
// (they were silently dropped before the cache-metering fix).
func TestStreamCachedContentTokenCount(t *testing.T) {
	raw, err := os.ReadFile("testdata/codeassist-stream-text.sse")
	if err != nil {
		t.Fatal(err)
	}
	sa := &StreamAdapter{Wrapped: true}
	var usage *engine.StreamUsage
	for ev := range sa.ParseStream(strings.NewReader(string(raw))) {
		if ev.Usage != nil {
			usage = ev.Usage
		}
	}
	if usage == nil {
		t.Fatal("no usage event emitted")
	}
	if usage.CacheReadTokens != 24430 {
		t.Errorf("CacheReadTokens = %d, want 24430", usage.CacheReadTokens)
	}
	if usage.InputTokens != 32179 || usage.OutputTokens != 196 {
		t.Errorf("input/output = %d/%d, want 32179/196", usage.InputTokens, usage.OutputTokens)
	}
}
