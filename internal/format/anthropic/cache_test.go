package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// TestCacheControlRoundTrip: cache_control markers set the way Claude Code
// sets them — last system block, last tool def, and the trailing user turns —
// must survive Unmarshal → Marshal. Dropping any of them disables Anthropic
// prompt caching for the whole prefix (the pre-fix behavior).
func TestCacheControlRoundTrip(t *testing.T) {
	adapter := &Adapter{}
	input := `{
		"model": "claude-sonnet-4-20250514",
		"max_tokens": 1024,
		"system": [
			{"type": "text", "text": "You are helpful.", "cache_control": {"type": "ephemeral"}}
		],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hi"}]},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_1", "name": "read", "input": {"path": "a.go"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "package a", "cache_control": {"type": "ephemeral"}}
			]},
			{"role": "user", "content": [
				{"type": "text", "text": "now fix it", "cache_control": {"type": "ephemeral", "ttl": "1h"}}
			]}
		],
		"tools": [
			{"name": "read", "input_schema": {"type": "object"}},
			{"name": "write", "input_schema": {"type": "object"}, "cache_control": {"type": "ephemeral"}}
		]
	}`

	chat, err := adapter.Unmarshal([]byte(input))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// IR captured the markers.
	if chat.Messages[0].Role != engine.RoleSystem || chat.Messages[0].CacheControl == nil {
		t.Errorf("system cache_control not captured: %+v", chat.Messages[0])
	}
	if chat.Tools[1].CacheControl == nil {
		t.Errorf("tool cache_control not captured: %+v", chat.Tools[1])
	}

	out, err := adapter.Marshal(chat)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var ar struct {
		System []map[string]any `json:"system"`
		Tools  []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(out, &ar); err != nil {
		t.Fatalf("re-parse: %v", err)
	}

	if ar.System[len(ar.System)-1]["cache_control"] == nil {
		t.Errorf("system cache_control dropped on marshal: %s", out)
	}
	foundTool := false
	for _, td := range ar.Tools {
		if td["name"] == "write" && td["cache_control"] != nil {
			foundTool = true
		}
	}
	if !foundTool {
		t.Errorf("tool cache_control dropped on marshal: %s", out)
	}

	// Exactly the input's breakpoints re-emitted — count them (4 in, ≤4 allowed).
	n := strings.Count(string(out), `"cache_control"`)
	if n != 4 {
		t.Errorf("expected 4 cache_control markers on the wire, got %d: %s", n, out)
	}
	// The 1h TTL variant passes through verbatim (opaque map).
	if !strings.Contains(string(out), `"ttl":"1h"`) {
		t.Errorf("cache_control ttl variant not preserved verbatim: %s", out)
	}
}

// TestStreamUsageCacheTokens: cache_creation/cache_read counts reported on
// message_start must ride the canonical Usage event and be re-serialized in
// the closing message_delta frame instead of being silently zeroed.
func TestStreamUsageCacheTokens(t *testing.T) {
	sa := &StreamAdapter{}
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":42,"cache_read_input_tokens":9000,"cache_creation_input_tokens":1200}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		``,
	}, "\n")

	var usage *engine.StreamUsage
	for ev := range sa.ParseStream(strings.NewReader(sse)) {
		if ev.Usage != nil {
			usage = ev.Usage
		}
	}
	if usage == nil {
		t.Fatal("no usage event emitted")
	}
	if usage.InputTokens != 42 || usage.OutputTokens != 5 {
		t.Errorf("input/output = %d/%d, want 42/5", usage.InputTokens, usage.OutputTokens)
	}
	if usage.CacheReadTokens != 9000 || usage.CacheWriteTokens != 1200 {
		t.Errorf("cache read/write = %d/%d, want 9000/1200", usage.CacheReadTokens, usage.CacheWriteTokens)
	}

	// Serializer re-emits the cache counts in Anthropic shape.
	got := usageJSON(usage)
	if !strings.Contains(got, `"cache_read_input_tokens":9000`) || !strings.Contains(got, `"cache_creation_input_tokens":1200`) {
		t.Errorf("serialized usage missing cache counts: %s", got)
	}
	// Zero cache counts keep the classic two-field shape.
	if got := usageJSON(&engine.StreamUsage{InputTokens: 1, OutputTokens: 2}); strings.Contains(got, "cache") {
		t.Errorf("uncached usage should omit cache fields: %s", got)
	}
}
