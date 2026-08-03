package engine

import (
	"crypto/sha256"
	"reflect"
	"testing"

	plugin_sdk "github.com/torana-edge/torana-plugin-sdk"
)

// Review round 3 finding 1 reproductions: computing a nested cache key must
// never mutate the live request (prefixMessage aliased the caller's
// *ToolResultBlock and truncated its content slice), and the key must equal
// an INDEPENDENT reference model.

func nestedMarkerRequest() *ChatRequest {
	return &ChatRequest{Model: "m", Messages: msgs(Message{Role: RoleUser, Blocks: []Block{
		{Text: &TextBlock{Text: "a"}},
		{ToolResult: &ToolResultBlock{
			ToolCallID: "c1",
			Content: []ToolResultContentBlock{
				{Text: "x"},
				{CacheBreakpoint: &CacheBreakpointBlock{Marker: mustMarker(`{"type":"ephemeral"}`)}},
				{Text: "y"},
			},
		}},
		{Text: &TextBlock{Text: "after"}},
	}})}
}

// TestCachePrefixKeyDoesNotMutateInput — the complete request must be
// byte/deep-equal before and after one and repeated CachePrefixKey calls.
func TestCachePrefixKeyDoesNotMutateInput(t *testing.T) {
	chat := nestedMarkerRequest()
	before := cloneChatRequest(chat)
	for i := 0; i < 3; i++ {
		if CachePrefixKey(chat) == "" {
			t.Fatal("key empty")
		}
	}
	if !reflect.DeepEqual(chat, before) {
		t.Fatal("CachePrefixKey mutated the live request")
	}
	// The suffix nested content must survive: content[2] is "y".
	tr := chat.Messages[0].Blocks[1].ToolResult
	if len(tr.Content) != 3 || tr.Content[2].Text != "y" {
		t.Fatalf("nested content truncated by the key computation: %+v", tr.Content)
	}
}

func cloneChatRequest(c *ChatRequest) *ChatRequest {
	out := *c
	out.Messages = append([]Message(nil), c.Messages...)
	for i := range out.Messages {
		out.Messages[i].Blocks = append([]Block(nil), c.Messages[i].Blocks...)
		for j := range out.Messages[i].Blocks {
			b := &out.Messages[i].Blocks[j]
			if b.ToolResult != nil {
				tr := *b.ToolResult
				tr.Content = append([]ToolResultContentBlock(nil), b.ToolResult.Content...)
				b.ToolResult = &tr
			}
		}
	}
	out.Tools = append([]ToolDef(nil), c.Tools...)
	return &out
}

// TestCachePrefixKeyAliasAdversarial — two requests intentionally SHARE
// block/tool-result pointers; computing one key must not mutate the other.
func TestCachePrefixKeyAliasAdversarial(t *testing.T) {
	shared := &ToolResultBlock{
		ToolCallID: "c1",
		Content: []ToolResultContentBlock{
			{Text: "x"},
			{CacheBreakpoint: &CacheBreakpointBlock{Marker: mustMarker(`{"type":"ephemeral"}`)}},
			{Text: "y"},
		},
	}
	a := &ChatRequest{Model: "m", Messages: msgs(Message{Role: RoleUser, Blocks: []Block{
		{Text: &TextBlock{Text: "a"}},
		{ToolResult: shared},
	}})}
	b := &ChatRequest{Model: "m", Messages: msgs(Message{Role: RoleUser, Blocks: []Block{
		{Text: &TextBlock{Text: "a"}},
		{ToolResult: shared},
	}})}
	if CachePrefixKey(a) == "" {
		t.Fatal("key empty")
	}
	if len(shared.Content) != 3 || shared.Content[2].Text != "y" {
		t.Fatal("computing one key mutated the shared tool-result of the other request")
	}
	if CachePrefixKey(a) != CachePrefixKey(b) {
		t.Fatal("alias requests must still produce equal keys")
	}
}

// referencePrefixKey is the INDEPENDENT reference model: it re-derives the
// expected framed hash from the raw fields, truncating messages with its
// OWN logic (never the production prefixMessage).
func referencePrefixKey(c *ChatRequest) string {
	h := sha256.New()
	writeHashField(h, domainCachePrefix)
	writeHashField(h, c.Model)

	last := lastCacheMarker(c)
	toolLimit := len(c.Tools)
	if last != nil && last.kind == markerCarrierTool {
		toolLimit = last.tool + 1
	}
	for i, t := range c.Tools {
		if i >= toolLimit {
			break
		}
		writeHashField(h, "tool")
		writeHashField(h, t.Name)
		writeHashField(h, t.Description)
		writeHashFieldBytes(h, t.Parameters.Bytes())
		writeHashFieldBytes(h, t.CacheControl.Bytes())
	}
	writeHashFieldBytes(h, c.ProviderExtensions.Bytes())
	writeHashFieldBytes(h, c.SafetySettings.Bytes())

	if last == nil {
		for i := range c.Messages {
			writeHashField(h, "msg")
			pbMsg, err := MessageToPB(&c.Messages[i])
			if err != nil {
				return ""
			}
			writeHashField(h, plugin_sdk.RequestBlocksFingerprint(pbMsg))
		}
		return shortHex(h)
	}
	if last.kind == markerCarrierTool {
		return shortHex(h)
	}
	for i := range c.Messages {
		if i > last.msg {
			break
		}
		if i < last.msg {
			writeHashField(h, "msg")
			pbMsg, err := MessageToPB(&c.Messages[i])
			if err != nil {
				return ""
			}
			writeHashField(h, plugin_sdk.RequestBlocksFingerprint(pbMsg))
			continue
		}
		// Independent truncation: rebuild the message from scratch with the
		// marker as the final element.
		m := c.Messages[i]
		trunc := Message{Role: m.Role}
		for j, b := range m.Blocks {
			if j > last.block {
				break
			}
			if j == last.block && last.nested >= 0 && b.ToolResult != nil {
				nb := b
				tr := *b.ToolResult
				tr.Content = append([]ToolResultContentBlock(nil), b.ToolResult.Content[:last.nested+1]...)
				nb.ToolResult = &tr
				trunc.Blocks = append(trunc.Blocks, nb)
				continue
			}
			trunc.Blocks = append(trunc.Blocks, b)
		}
		writeHashField(h, "msg")
		pbMsg, err := MessageToPB(&trunc)
		if err != nil {
			return ""
		}
		writeHashField(h, plugin_sdk.RequestBlocksFingerprint(pbMsg))
	}
	return shortHex(h)
}

// TestCachePrefixKeyMatchesReferenceModel — production equals the
// independent reference for top-level, nested, tool, multiple-marker, and
// no-marker cases.
func TestCachePrefixKeyMatchesReferenceModel(t *testing.T) {
	cases := map[string]*ChatRequest{
		"nested marker": nestedMarkerRequest(),
		"top-level marker": {Model: "m", Messages: msgs(Message{Role: RoleUser, Blocks: []Block{
			{Text: &TextBlock{Text: "a"}},
			{CacheBreakpoint: &CacheBreakpointBlock{Marker: mustMarker(`{"type":"ephemeral"}`)}},
			{Text: &TextBlock{Text: "after"}},
		}})},
		"tool marker": {Model: "m", Messages: msgs(userMsg("u")),
			Tools: []ToolDef{{Name: "read", Parameters: mustReqForTest(`{"type":"object"}`), CacheControl: mustOptForTest(`{"type":"ephemeral"}`)}}},
		"multiple markers (message wins)": {Model: "m",
			Tools:    []ToolDef{{Name: "read", Parameters: mustReqForTest(`{}`), CacheControl: mustOptForTest(`{"type":"ephemeral"}`)}},
			Messages: msgs(userMsg("u"), userMsg("v"))},
		"no marker": {Model: "m", Messages: msgs(userMsg("a"), userMsg("b"))},
		"nested marker in second message": {Model: "m", Messages: msgs(
			userMsg("first"),
			Message{Role: RoleUser, Blocks: []Block{
				{Text: &TextBlock{Text: "a"}},
				{ToolResult: &ToolResultBlock{
					ToolCallID: "c1",
					Content: []ToolResultContentBlock{
						{Text: "x"},
						{CacheBreakpoint: &CacheBreakpointBlock{Marker: mustMarker(`{"type":"ephemeral"}`)}},
						{Text: "y"},
					},
				}},
			}},
			userMsg("later"),
		)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := CachePrefixKey(c)
			want := referencePrefixKey(c)
			if got != want {
				t.Fatalf("production key %q != reference key %q", got, want)
			}
			if got == "" {
				t.Fatal("key unexpectedly empty")
			}
		})
	}
}

// TestCachePrefixKeyToolsOnlyRequests — the SDK domain permits zero
// messages (a tools-only request); the tool prefix must get a key, not the
// empty string.
func TestCachePrefixKeyToolsOnlyRequests(t *testing.T) {
	withMarker := &ChatRequest{Model: "m",
		Tools: []ToolDef{{Name: "read", Parameters: mustReqForTest(`{"type":"object"}`), CacheControl: mustOptForTest(`{"type":"ephemeral"}`)}}}
	if got := CachePrefixKey(withMarker); got == "" {
		t.Fatal("a tools-only request with a tool marker must key the tool prefix")
	}
	withMarkerChanged := &ChatRequest{Model: "m",
		Tools: []ToolDef{{Name: "read", Parameters: mustReqForTest(`{"type":"object"}`), CacheControl: mustOptForTest(`{"type":"ephemeral","ttl":"1h"}`)}}}
	if CachePrefixKey(withMarker) == CachePrefixKey(withMarkerChanged) {
		t.Fatal("the tool marker change must move the tools-only key")
	}
	// Without a marker the whole (tool) request is the automatic prefix.
	noMarker := &ChatRequest{Model: "m",
		Tools: []ToolDef{{Name: "read", Parameters: mustReqForTest(`{"type":"object"}`)}}}
	if got := CachePrefixKey(noMarker); got == "" {
		t.Fatal("a tools-only request without a marker must key the whole tool prefix")
	}
}
