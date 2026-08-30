package engine

import (
	"testing"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// The ordered-prefix cache identity (review round 2 finding 1): the prefix
// closes at the LAST marker in provider-visible serialization order — tools
// first, then messages, outer blocks, nested tool-result content.
// CachePrefixKey takes the validated PB request and is self-gated by the
// SDK's full-domain validator.

func pbUserMsg(text string) *pb.Message {
	return &pb.Message{Role: "user", Blocks: []*pb.RequestBlock{{
		Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: text}},
	}}}
}

func pbMarkerBytes(raw string) []byte { return []byte(raw) }

// TestCachePrefixKeyTopLevelMarkerTruncatesAtPosition — with a top-level
// marker mid-message, every field before/at it changes the key; every later
// block and message does not.
func TestCachePrefixKeyTopLevelMarkerTruncatesAtPosition(t *testing.T) {
	mk := func(before, after string) *pb.ChatRequest {
		return &pb.ChatRequest{Model: "m", Messages: []*pb.Message{{Role: "user", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: before}}},
			{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}},
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: after}}},
		}}, pbUserMsg("later")}}
	}
	base := mk("a", "z")
	changedBefore := mk("a'", "z")
	changedAfter := mk("a", "z'")
	if CachePrefixKey(base) == CachePrefixKey(changedBefore) {
		t.Fatal("a change BEFORE the marker must move the key")
	}
	if CachePrefixKey(base) != CachePrefixKey(changedAfter) {
		t.Fatal("a change AFTER the top-level marker must NOT move the key")
	}
	laterChanged := mk("a", "z")
	laterChanged.Messages[1] = pbUserMsg("later'")
	if CachePrefixKey(base) != CachePrefixKey(laterChanged) {
		t.Fatal("a change in a message AFTER the marker message must NOT move the key")
	}
}

// TestCachePrefixKeyNestedMarkerTruncatesAtPosition — a nested marker
// mid-tool-result: earlier nested content, result identity, enclosing
// message facts, and the marker change the key; later nested content, later
// outer blocks, and later messages do not.
func TestCachePrefixKeyNestedMarkerTruncatesAtPosition(t *testing.T) {
	mk := func(first, second string) *pb.ChatRequest {
		return &pb.ChatRequest{Model: "m", Messages: []*pb.Message{{Role: "user", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "prefix"}}},
			{Kind: &pb.RequestBlock_ToolResult{ToolResult: &pb.RequestToolResultBlock{
				ToolCallId: "c1",
				Content: []*pb.ToolResultContentBlock{
					{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: first}}},
					{Kind: &pb.ToolResultContentBlock_CacheBreakpoint{CacheBreakpoint: &pb.ToolResultCacheBreakpoint{MarkerJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}},
					{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: second}}},
				},
			}}},
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "outer-after"}}},
		}}}}
	}
	base := mk("a", "z")
	changedFirst := mk("a'", "z")
	changedSecond := mk("a", "z'")
	if CachePrefixKey(base) == CachePrefixKey(changedFirst) {
		t.Fatal("a nested change BEFORE the marker must move the key")
	}
	if CachePrefixKey(base) != CachePrefixKey(changedSecond) {
		t.Fatal("a nested change AFTER the marker must NOT move the key")
	}
	identityChanged := mk("a", "z")
	identityChanged.Messages[0].Blocks[1].GetToolResult().ToolCallId = "c2"
	if CachePrefixKey(base) == CachePrefixKey(identityChanged) {
		t.Fatal("the result identity (before the marker) must move the key")
	}
	outerChanged := mk("a", "z")
	outerChanged.Messages[0].Blocks[2].GetText().Text = "outer-after'"
	if CachePrefixKey(base) != CachePrefixKey(outerChanged) {
		t.Fatal("an outer block AFTER the nested marker must NOT move the key")
	}
	enclosingChanged := mk("a", "z")
	enclosingChanged.Messages[0].Blocks[0].GetText().Text = "prefix'"
	if CachePrefixKey(base) == CachePrefixKey(enclosingChanged) {
		t.Fatal("an enclosing message fact before the marker must move the key")
	}
	markerChanged := mk("a", "z")
	markerChanged.Messages[0].Blocks[1].GetToolResult().Content[1].GetCacheBreakpoint().MarkerJson = pbMarkerBytes(`{"type":"ephemeral","ttl":"1h"}`)
	if CachePrefixKey(base) == CachePrefixKey(markerChanged) {
		t.Fatal("the marker bytes must move the key")
	}
	// A message after the marker message does not move the key.
	base2 := mk("a", "z")
	base2.Messages = append(base2.Messages, pbUserMsg("later"))
	later := mk("a", "z")
	later.Messages = append(later.Messages, pbUserMsg("later'"))
	if CachePrefixKey(base2) != CachePrefixKey(later) {
		t.Fatal("a later message must NOT move the key")
	}
}

// TestCachePrefixKeyToolMarkerEndsPrefixAtTools — a tool-definition marker
// with no later message marker closes the prefix in the tools section:
// earlier tool facts and the marker change the key; later tools and ALL
// messages do not.
func TestCachePrefixKeyToolMarkerEndsPrefixAtTools(t *testing.T) {
	mk := func(extraTool bool) *pb.ChatRequest {
		c := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{pbUserMsg("u")}}
		c.Tools = []*pb.ToolDef{{Name: "read", ParametersJson: pbMarkerBytes(`{"type":"object"}`), CacheControlJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}
		if extraTool {
			c.Tools = append(c.Tools, &pb.ToolDef{Name: "extra", ParametersJson: pbMarkerBytes(`{"type":"object"}`)})
		}
		return c
	}
	base := mk(false)
	changedEarlier := mk(false)
	changedEarlier.Tools[0].Description = "changed"
	if CachePrefixKey(base) == CachePrefixKey(changedEarlier) {
		t.Fatal("an earlier tool fact must move the key")
	}
	changedMarker := mk(false)
	changedMarker.Tools[0].CacheControlJson = pbMarkerBytes(`{"type":"ephemeral","ttl":"1h"}`)
	if CachePrefixKey(base) == CachePrefixKey(changedMarker) {
		t.Fatal("the tool marker bytes must move the key")
	}
	if CachePrefixKey(base) != CachePrefixKey(mk(true)) {
		t.Fatal("a tool AFTER the marker must NOT move the key")
	}
	msgChanged := mk(false)
	msgChanged.Messages[0] = pbUserMsg("u'")
	if CachePrefixKey(base) != CachePrefixKey(msgChanged) {
		t.Fatal("a message change must NOT move the key when the prefix ended in the tools section")
	}
}

// TestCachePrefixKeyLastMarkerWins — multiple markers select the last
// serialized marker; a message marker supersedes an earlier tool marker.
func TestCachePrefixKeyLastMarkerWins(t *testing.T) {
	mk := func(msg string) *pb.ChatRequest {
		c := &pb.ChatRequest{Model: "m"}
		c.Tools = []*pb.ToolDef{{Name: "read", ParametersJson: pbMarkerBytes(`{}`), CacheControlJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}
		c.Messages = []*pb.Message{
			pbUserMsg("u"),
			{Role: "user", Blocks: []*pb.RequestBlock{
				{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: msg}}},
				{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}},
			}},
		}
		return c
	}
	base := mk("a")
	laterTool := mk("a")
	laterTool.Tools = append(laterTool.Tools, &pb.ToolDef{Name: "extra", ParametersJson: pbMarkerBytes(`{}`)})
	if CachePrefixKey(base) == CachePrefixKey(laterTool) {
		t.Fatal("with a later message marker, all tools are in the prefix — a tool change must move the key")
	}
	after := mk("a")
	after.Messages[1].Blocks = append(after.Messages[1].Blocks, &pb.RequestBlock{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "after"}}})
	if CachePrefixKey(base) != CachePrefixKey(after) {
		t.Fatal("content after the LAST marker must NOT move the key")
	}
}

// TestCachePrefixKeyAbsentMarkerHashesEverything — no marker anywhere: the
// automatic-cache prefix is the whole request.
func TestCachePrefixKeyAbsentMarkerHashesEverything(t *testing.T) {
	base := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{pbUserMsg("a"), pbUserMsg("b")}}
	mutated := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{pbUserMsg("a"), pbUserMsg("b'")}}
	if CachePrefixKey(base) == CachePrefixKey(mutated) {
		t.Fatal("without a marker, any message change must move the key")
	}
}

// TestCachePrefixKeyCarrierPositionsDistinct — the same marker bytes at
// different carriers/positions remain distinct keys.
func TestCachePrefixKeyCarrierPositionsDistinct(t *testing.T) {
	topLevel := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{{Role: "user", Blocks: []*pb.RequestBlock{
		{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "a"}}},
		{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}},
	}}}}
	nested := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{{Role: "user", Blocks: []*pb.RequestBlock{
		{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "a"}}},
		{Kind: &pb.RequestBlock_ToolResult{ToolResult: &pb.RequestToolResultBlock{
			ToolCallId: "c1",
			Content: []*pb.ToolResultContentBlock{
				{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "x"}}},
				{Kind: &pb.ToolResultContentBlock_CacheBreakpoint{CacheBreakpoint: &pb.ToolResultCacheBreakpoint{MarkerJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}},
			},
		}}},
	}}}}
	tool := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{pbUserMsg("a")},
		Tools: []*pb.ToolDef{{Name: "read", ParametersJson: pbMarkerBytes(`{}`), CacheControlJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}}
	keys := map[string]string{"top": CachePrefixKey(topLevel), "nested": CachePrefixKey(nested), "tool": CachePrefixKey(tool)}
	if keys["top"] == keys["nested"] || keys["top"] == keys["tool"] || keys["nested"] == keys["tool"] {
		t.Fatalf("identical marker bytes at different carriers collided: %v", keys)
	}
}

// TestCachePrefixKeyToolsOnlyRequests — the SDK domain permits zero
// messages (a tools-only request); the tool prefix must get a key, not the
// empty string.
func TestCachePrefixKeyToolsOnlyRequests(t *testing.T) {
	withMarker := &pb.ChatRequest{Model: "m",
		Tools: []*pb.ToolDef{{Name: "read", ParametersJson: pbMarkerBytes(`{"type":"object"}`), CacheControlJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}}
	if got := CachePrefixKey(withMarker); got == "" {
		t.Fatal("a tools-only request with a tool marker must key the tool prefix")
	}
	withMarkerChanged := &pb.ChatRequest{Model: "m",
		Tools: []*pb.ToolDef{{Name: "read", ParametersJson: pbMarkerBytes(`{"type":"object"}`), CacheControlJson: pbMarkerBytes(`{"type":"ephemeral","ttl":"1h"}`)}}}
	if CachePrefixKey(withMarker) == CachePrefixKey(withMarkerChanged) {
		t.Fatal("the tool marker change must move the tools-only key")
	}
	noMarker := &pb.ChatRequest{Model: "m",
		Tools: []*pb.ToolDef{{Name: "read", ParametersJson: pbMarkerBytes(`{"type":"object"}`)}}}
	if got := CachePrefixKey(noMarker); got == "" {
		t.Fatal("a tools-only request without a marker must key the whole tool prefix")
	}
}
