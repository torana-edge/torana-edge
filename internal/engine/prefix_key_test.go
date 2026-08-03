package engine

import (
	"testing"

	plugin_sdk "github.com/torana-edge/torana-plugin-sdk"
)

// The ordered-prefix cache identity (review round 2 finding 1): the prefix
// closes at the LAST marker in provider-visible serialization order — tools
// first, then messages, outer blocks, nested tool-result content. These
// rows were written BEFORE the CachePrefixKey rewrite: message-count
// truncation saw only top-level blocks and ignored tool markers.

func userMsg(text string) Message {
	return Message{Role: RoleUser, Blocks: []Block{{Text: &TextBlock{Text: text}}}}
}

// TestCachePrefixKeyTopLevelMarkerTruncatesAtPosition — with a top-level
// marker mid-message, every field before/at it changes the key; every later
// block and message does not.
func TestCachePrefixKeyTopLevelMarkerTruncatesAtPosition(t *testing.T) {
	mk := func(before, after string) *ChatRequest {
		blocks := []Block{{Text: &TextBlock{Text: before}}}
		blocks = append(blocks, Block{CacheBreakpoint: &CacheBreakpointBlock{Marker: mustMarker(`{"type":"ephemeral"}`)}})
		blocks = append(blocks, Block{Text: &TextBlock{Text: after}})
		return &ChatRequest{Model: "m", Messages: msgs(
			Message{Role: RoleUser, Blocks: blocks},
			userMsg("later"),
		)}
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
	laterChanged.Messages[1] = userMsg("later'")
	if CachePrefixKey(base) != CachePrefixKey(laterChanged) {
		t.Fatal("a change in a message AFTER the marker message must NOT move the key")
	}
}

// TestCachePrefixKeyNestedMarkerTruncatesAtPosition — a nested marker
// mid-tool-result: earlier nested content, result identity, enclosing
// message facts, and the marker change the key; later nested content, later
// outer blocks, and later messages do not.
func TestCachePrefixKeyNestedMarkerTruncatesAtPosition(t *testing.T) {
	mk := func(first, second string) *ChatRequest {
		return &ChatRequest{Model: "m", Messages: msgs(Message{Role: RoleUser, Blocks: []Block{
			{Text: &TextBlock{Text: "prefix"}},
			{ToolResult: &ToolResultBlock{
				ToolCallID: "c1",
				Content: []ToolResultContentBlock{
					{Text: first},
					{CacheBreakpoint: &CacheBreakpointBlock{Marker: mustMarker(`{"type":"ephemeral"}`)}},
					{Text: second},
				},
			}},
			{Text: &TextBlock{Text: "outer-after"}},
		}})}
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
	identityChanged.Messages[0].Blocks[1].ToolResult.ToolCallID = "c2"
	if CachePrefixKey(base) == CachePrefixKey(identityChanged) {
		t.Fatal("the result identity (before the marker) must move the key")
	}
	outerChanged := mk("a", "z")
	outerChanged.Messages[0].Blocks[2].Text.Text = "outer-after'"
	if CachePrefixKey(base) != CachePrefixKey(outerChanged) {
		t.Fatal("an outer block AFTER the nested marker must NOT move the key")
	}
	enclosingChanged := mk("a", "z")
	enclosingChanged.Messages[0].Blocks[0].Text.Text = "prefix'"
	if CachePrefixKey(base) == CachePrefixKey(enclosingChanged) {
		t.Fatal("an enclosing message fact before the marker must move the key")
	}
	markerChanged := mk("a", "z")
	markerChanged.Messages[0].Blocks[1].ToolResult.Content[1].CacheBreakpoint.Marker = mustMarker(`{"type":"ephemeral","ttl":"1h"}`)
	if CachePrefixKey(base) == CachePrefixKey(markerChanged) {
		t.Fatal("the marker bytes must move the key")
	}
	// A message after the marker message does not move the key.
	base2 := mk("a", "z")
	base2.Messages = append(base2.Messages, userMsg("later"))
	later := mk("a", "z")
	later.Messages = append(later.Messages, userMsg("later'"))
	if CachePrefixKey(base2) != CachePrefixKey(later) {
		t.Fatal("a later message must NOT move the key")
	}
}

// TestCachePrefixKeyToolMarkerEndsPrefixAtTools — a tool-definition marker
// with no later message marker closes the prefix in the tools section:
// earlier tool facts and the marker change the key; later tools and ALL
// messages do not.
func TestCachePrefixKeyToolMarkerEndsPrefixAtTools(t *testing.T) {
	mk := func(extraTool bool) *ChatRequest {
		c := &ChatRequest{Model: "m", Messages: msgs(userMsg("u"))}
		c.Tools = []ToolDef{{Name: "read", Parameters: mustReqForTest(`{"type":"object"}`), CacheControl: mustOptForTest(`{"type":"ephemeral"}`)}}
		if extraTool {
			c.Tools = append(c.Tools, ToolDef{Name: "extra", Parameters: mustReqForTest(`{"type":"object"}`)})
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
	changedMarker.Tools[0].CacheControl = mustOptForTest(`{"type":"ephemeral","ttl":"1h"}`)
	if CachePrefixKey(base) == CachePrefixKey(changedMarker) {
		t.Fatal("the tool marker bytes must move the key")
	}
	if CachePrefixKey(base) != CachePrefixKey(mk(true)) {
		t.Fatal("a tool AFTER the marker must NOT move the key")
	}
	msgChanged := mk(false)
	msgChanged.Messages[0] = userMsg("u'")
	if CachePrefixKey(base) != CachePrefixKey(msgChanged) {
		t.Fatal("a message change must NOT move the key when the prefix ended in the tools section")
	}
}

// TestCachePrefixKeyLastMarkerWins — multiple markers select the last
// serialized marker; a message marker supersedes an earlier tool marker.
func TestCachePrefixKeyLastMarkerWins(t *testing.T) {
	// Tool marker + later message marker: the message marker wins, so tools
	// after the tool marker DO move the key (all tools precede messages).
	mk := func(msg string) *ChatRequest {
		c := &ChatRequest{Model: "m"}
		c.Tools = []ToolDef{{Name: "read", Parameters: mustReqForTest(`{}`), CacheControl: mustOptForTest(`{"type":"ephemeral"}`)}}
		c.Messages = msgs(
			userMsg("u"),
			Message{Role: RoleUser, Blocks: []Block{
				{Text: &TextBlock{Text: msg}},
				{CacheBreakpoint: &CacheBreakpointBlock{Marker: mustMarker(`{"type":"ephemeral"}`)}},
			}},
		)
		return c
	}
	base := mk("a")
	laterTool := mk("a")
	laterTool.Tools = append(laterTool.Tools, ToolDef{Name: "extra", Parameters: mustReqForTest(`{}`)})
	if CachePrefixKey(base) == CachePrefixKey(laterTool) {
		t.Fatal("with a later message marker, all tools are in the prefix — a tool change must move the key")
	}
	after := mk("a")
	after.Messages[1].Blocks = append(after.Messages[1].Blocks, Block{Text: &TextBlock{Text: "after"}})
	if CachePrefixKey(base) != CachePrefixKey(after) {
		t.Fatal("content after the LAST marker must NOT move the key")
	}
}

// TestCachePrefixKeyAbsentMarkerHashesEverything — no marker anywhere: the
// automatic-cache prefix is the whole request.
func TestCachePrefixKeyAbsentMarkerHashesEverything(t *testing.T) {
	base := &ChatRequest{Model: "m", Messages: msgs(userMsg("a"), userMsg("b"))}
	mutated := &ChatRequest{Model: "m", Messages: msgs(userMsg("a"), userMsg("b'"))}
	if CachePrefixKey(base) == CachePrefixKey(mutated) {
		t.Fatal("without a marker, any message change must move the key")
	}
}

// TestCachePrefixKeyCarrierPositionsDistinct — the same marker bytes at
// different carriers/positions remain distinct keys.
func TestCachePrefixKeyCarrierPositionsDistinct(t *testing.T) {
	topLevel := &ChatRequest{Model: "m", Messages: msgs(Message{Role: RoleUser, Blocks: []Block{
		{Text: &TextBlock{Text: "a"}},
		{CacheBreakpoint: &CacheBreakpointBlock{Marker: mustMarker(`{"type":"ephemeral"}`)}},
	}})}
	nested := &ChatRequest{Model: "m", Messages: msgs(Message{Role: RoleUser, Blocks: []Block{
		{Text: &TextBlock{Text: "a"}},
		{ToolResult: &ToolResultBlock{
			ToolCallID: "c1",
			Content: []ToolResultContentBlock{
				{Text: "x"},
				{CacheBreakpoint: &CacheBreakpointBlock{Marker: mustMarker(`{"type":"ephemeral"}`)}},
			},
		}},
	}})}
	tool := &ChatRequest{Model: "m", Messages: msgs(userMsg("a")),
		Tools: []ToolDef{{Name: "read", Parameters: mustReqForTest(`{}`), CacheControl: mustOptForTest(`{"type":"ephemeral"}`)}}}
	keys := map[string]string{"top": CachePrefixKey(topLevel), "nested": CachePrefixKey(nested), "tool": CachePrefixKey(tool)}
	if keys["top"] == keys["nested"] || keys["top"] == keys["tool"] || keys["nested"] == keys["tool"] {
		t.Fatalf("identical marker bytes at different carriers collided: %v", keys)
	}
}

// TestCachePrefixKeyReferenceProjection — the host key's message section is
// the SDK's shared fingerprint over the TRUNCATED message (the reference
// projection the tier-selector mirror must agree with on the shared
// supported domain).
func TestCachePrefixKeyReferenceProjection(t *testing.T) {
	chat := &ChatRequest{Model: "m", Messages: msgs(Message{Role: RoleUser, Blocks: []Block{
		{Text: &TextBlock{Text: "a"}},
		{ToolResult: &ToolResultBlock{
			ToolCallID: "c1",
			Content: []ToolResultContentBlock{
				{Text: "x"},
				{CacheBreakpoint: &CacheBreakpointBlock{Marker: mustMarker(`{"type":"ephemeral"}`)}},
				{Text: "y"},
			},
		}},
	}}, userMsg("later"))}

	// The reference projection: full messages before the marker message, the
	// marker message truncated at the nested marker (inclusive).
	truncated := Message{Role: RoleUser, Blocks: []Block{
		{Text: &TextBlock{Text: "a"}},
		{ToolResult: &ToolResultBlock{
			ToolCallID: "c1",
			Content: []ToolResultContentBlock{
				{Text: "x"},
				{CacheBreakpoint: &CacheBreakpointBlock{Marker: mustMarker(`{"type":"ephemeral"}`)}},
			},
		}},
	}}
	pbTrunc, err := MessageToPB(&truncated)
	if err != nil {
		t.Fatalf("MessageToPB: %v", err)
	}
	if got := plugin_sdk.RequestBlocksFingerprint(pbTrunc); got == "" {
		t.Fatal("reference fingerprint empty")
	}
	// The key must be stable when only the truncated section is replayed:
	// appending a turn AFTER the marker message must not move it.
	turn2 := &ChatRequest{Model: "m", Messages: append(append([]Message{}, chat.Messages...), userMsg("next"))}
	if CachePrefixKey(chat) != CachePrefixKey(turn2) {
		t.Fatal("a turn appended after the marker message must not move the key")
	}
}

func mustMarker(raw string) RequiredJSONObject {
	r, err := ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		panic(err)
	}
	return r
}

func mustReqForTest(raw string) RequiredJSONObject {
	r, err := ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		panic(err)
	}
	return r
}

func mustOptForTest(raw string) OptionalJSONObject {
	r, err := ParseOptionalJSONObject([]byte(raw))
	if err != nil {
		panic(err)
	}
	return r
}
