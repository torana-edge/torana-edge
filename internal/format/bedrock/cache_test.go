package bedrock

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// TestCachePointRoundTrip: Converse cachePoint blocks — in system, in message
// content, and as a toolConfig.tools entry — must survive Unmarshal → Marshal.
func TestCachePointRoundTrip(t *testing.T) {
	adapter := &Adapter{}
	input := `{
		"modelId": "anthropic.claude-sonnet-4-20250514-v1:0",
		"system": [
			{"text": "You are helpful."},
			{"cachePoint": {"type": "default"}}
		],
		"messages": [
			{"role": "user", "content": [
				{"text": "hello"},
				{"cachePoint": {"type": "default"}}
			]},
			{"role": "assistant", "content": [{"text": "hi"}]}
		],
		"toolConfig": {"tools": [
			{"toolSpec": {"name": "read", "inputSchema": {"json": {"type": "object"}}}},
			{"cachePoint": {"type": "default"}}
		]}
	}`

	chat, err := adapter.Unmarshal([]byte(input))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if chat.Messages[0].Role != engine.RoleSystem || chat.Messages[0].CacheControl == nil {
		t.Errorf("system cachePoint not captured: %+v", chat.Messages[0])
	}
	if chat.Messages[1].CacheControl == nil {
		t.Errorf("user message cachePoint not captured: %+v", chat.Messages[1])
	}
	// The system text must not gain stray newlines from the cachePoint entry.
	if chat.Messages[0].Content != "You are helpful." {
		t.Errorf("system text polluted by cachePoint entry: %q", chat.Messages[0].Content)
	}
	if len(chat.Tools) != 1 || chat.Tools[0].CacheControl == nil {
		t.Errorf("tool cachePoint not captured: %+v", chat.Tools)
	}

	out, err := adapter.Marshal(chat)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	n := strings.Count(string(out), `"cachePoint"`)
	if n != 3 {
		t.Errorf("expected 3 cachePoint blocks on the wire, got %d: %s", n, out)
	}

	// The tools cachePoint is its own entry after the toolSpec, and the
	// toolSpec entry has no stray cachePoint key.
	var req struct {
		ToolConfig struct {
			Tools []map[string]any `json:"tools"`
		} `json:"toolConfig"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if len(req.ToolConfig.Tools) != 2 {
		t.Fatalf("expected [toolSpec, cachePoint] entries, got %d: %s", len(req.ToolConfig.Tools), out)
	}
	if req.ToolConfig.Tools[0]["cachePoint"] != nil || req.ToolConfig.Tools[1]["cachePoint"] == nil {
		t.Errorf("tools cachePoint entry misplaced: %s", out)
	}
}

// TestStreamMetadataCacheTokens: ConverseStream metadata cache counts flow
// into the canonical usage event and back out on serialization.
func TestStreamMetadataCacheTokens(t *testing.T) {
	var inThinking, inToolUse bool
	var sigBuf string
	ev := parseBedrockEvent(`{"metadata":{"usage":{"inputTokens":10,"outputTokens":4,"totalTokens":14,"cacheReadInputTokens":8000,"cacheWriteInputTokens":500}}}`, &inThinking, &inToolUse, &sigBuf)
	if ev == nil || ev.Usage == nil {
		t.Fatal("no usage event from metadata")
	}
	if ev.Usage.CacheReadTokens != 8000 || ev.Usage.CacheWriteTokens != 500 {
		t.Errorf("cache read/write = %d/%d, want 8000/500", ev.Usage.CacheReadTokens, ev.Usage.CacheWriteTokens)
	}

	frames := marshalStreamEvent(engine.StreamEvent{Usage: ev.Usage})
	if len(frames) == 0 {
		t.Fatal("no serialized frame")
	}
	if !strings.Contains(frames[0], `"cacheReadInputTokens":8000`) || !strings.Contains(frames[0], `"cacheWriteInputTokens":500`) {
		t.Errorf("serialized metadata missing cache counts: %s", frames[0])
	}
}
