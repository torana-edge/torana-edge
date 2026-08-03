package format_test

import (
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format/anthropic"
	"github.com/torana-edge/torana-edge/internal/format/bedrock"
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
	if engine.CachePrefixKey(chat) == "" {
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

	// Bedrock: nested markers are not representable on its wire (fail-closed
	// adapter rule), so the bedrock leg uses a top-level marker — the suffix
	// blocks after it must reach the wire after the key computation.
	bedChat := &engine.ChatRequest{
		Model: "m",
		Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{
			{Text: &engine.TextBlock{Text: "prefix"}},
			{CacheBreakpoint: &engine.CacheBreakpointBlock{Marker: mustMarkerForTest(`{"type":"ephemeral"}`)}},
			{Text: &engine.TextBlock{Text: "outer-suffix"}},
		}}},
	}
	if engine.CachePrefixKey(bedChat) == "" {
		t.Fatal("key empty")
	}
	bed, err := (&bedrock.Adapter{}).Marshal(bedChat)
	if err != nil {
		t.Fatalf("bedrock marshal: %v", err)
	}
	if !strings.Contains(string(bed), "outer-suffix") {
		t.Fatalf("bedrock suffix block missing after key computation: %s", bed)
	}
}

func mustMarkerForTest(raw string) engine.RequiredJSONObject {
	r, err := engine.ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		panic(err)
	}
	return r
}
