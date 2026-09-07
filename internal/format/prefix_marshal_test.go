package format_test

import (
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/format/anthropic"
)

// Review round 3 finding 1: after CachePrefixKey computes the key, every
// suffix block must still reach the provider wire — the key computation
// must never truncate the live request.
func TestMarshalAfterCachePrefixKeyKeepsSuffix(t *testing.T) {
	chat := &engine.ChatRequest{
		Model: "m",
		Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{
			{Text: &engine.TextBlock{Text: "prefix"}},
			{ToolResult: &engine.ToolResultBlock{
				ToolCallID: "c1",
				Content: []engine.ToolResultContentBlock{
					{Text: "before-marker"},
					{CacheBreakpoint: &engine.CacheBreakpointBlock{Marker: mustMarkerForTest(`{"type":"ephemeral"}`)}},
					{Text: "after-marker-suffix"},
				},
			}},
			{Text: &engine.TextBlock{Text: "outer-suffix"}},
		}}},
	}
	if mustCacheKey(t, chat) == "" {
		t.Fatal("key empty")
	}

	// Anthropic: the nested marker becomes a block-level cache_control on
	// the tool_result; the suffix nested text and the outer text must both
	// be on the wire.
	anth, err := (&anthropic.Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatalf("anthropic marshal: %v", err)
	}
	if !strings.Contains(string(anth), "after-marker-suffix") || !strings.Contains(string(anth), "outer-suffix") {
		t.Fatalf("suffix blocks missing after key computation: %s", anth)
	}

	// The same for a TOP-LEVEL marker rather than a nested one: the blocks
	// after it must still reach the wire once the key has been computed.
	topChat := &engine.ChatRequest{
		Model: "m",
		Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{
			{Text: &engine.TextBlock{Text: "prefix"}},
			{CacheBreakpoint: &engine.CacheBreakpointBlock{Marker: mustMarkerForTest(`{"type":"ephemeral"}`)}},
			{Text: &engine.TextBlock{Text: "outer-suffix"}},
		}}},
	}
	if mustCacheKey(t, topChat) == "" {
		t.Fatal("key empty")
	}
	top, err := (&anthropic.Adapter{}).Marshal(topChat)
	if err != nil {
		t.Fatalf("top-level marker marshal: %v", err)
	}
	if !strings.Contains(string(top), "outer-suffix") {
		t.Fatalf("suffix block missing after key computation: %s", top)
	}
}

func mustCacheKey(t *testing.T, chat *engine.ChatRequest) string {
	t.Helper()
	pbReq, err := pbconv.ToPBChatRequestChecked(chat)
	if err != nil {
		t.Fatalf("checked projection: %v", err)
	}
	return engine.CachePrefixKey(pbReq)
}

func mustMarkerForTest(raw string) engine.RequiredJSONObject {
	r, err := engine.ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		panic(err)
	}
	return r
}
