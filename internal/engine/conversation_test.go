package engine

import (
	"encoding/json"
	"testing"
)

func mustMeta(m map[string]any) OptionalJSONObject {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	r, err := ParseOptionalJSONObject(b)
	if err != nil {
		panic(err)
	}
	return r
}

func msgs(m ...Message) []Message { return m }

func textBlock(s string) Block { return Block{Text: &TextBlock{Text: s}} }

func sys(s string) Message  { return Message{Role: RoleSystem, Blocks: []Block{textBlock(s)}} }
func user(s string) Message { return Message{Role: RoleUser, Blocks: []Block{textBlock(s)}} }
func asst(s string) Message { return Message{Role: RoleAssistant, Blocks: []Block{textBlock(s)}} }

// ephemeral is an Anthropic-shaped breakpoint. Any format carrying the same
// shape through the IR — including a chat-completions provider that adopts it —
// exercises the identical code path.
func ephemeral() RequiredJSONObject {
	r, err := ParseRequiredJSONObject([]byte(`{"type":"ephemeral"}`))
	if err != nil {
		panic(err)
	}
	return r
}

// --- ConversationID: the durable label ---

// TestConversationIDStableAcrossTurns is the property the label rests on: as a
// conversation grows, it must not move. A moving label orphans a warm entry on
// every turn.
func TestConversationIDStableAcrossTurns(t *testing.T) {
	turn1 := &ChatRequest{Messages: msgs(sys("you are a helpful assistant"), user("refactor the loader"))}
	turn5 := &ChatRequest{Messages: msgs(
		sys("you are a helpful assistant"),
		user("refactor the loader"),
		asst("sure, which file?"),
		user("internal/plugin/discovery.go"),
		asst("done"),
	)}

	if got, want := ConversationID(turn5), ConversationID(turn1); got != want {
		t.Errorf("label moved as the conversation grew: turn5=%s turn1=%s", got, want)
	}
}

// TestConversationIDIgnoresModelAndTools pins the documented exclusions.
func TestConversationIDIgnoresModelAndTools(t *testing.T) {
	base := msgs(sys("prompt"), user("hello"))
	a := &ChatRequest{Model: "claude-sonnet-4-5", Messages: base}
	b := &ChatRequest{Model: "claude-opus-4-1", Messages: base, Tools: []ToolDef{{Name: "bash"}}}

	if ConversationID(a) != ConversationID(b) {
		t.Error("a model switch or an added tool renamed the conversation")
	}
}

// TestConversationIDDistinguishesConversations is the converse: two different
// conversations must not share a warm entry.
func TestConversationIDDistinguishesConversations(t *testing.T) {
	a := &ChatRequest{Messages: msgs(sys("prompt"), user("refactor the loader"))}
	b := &ChatRequest{Messages: msgs(sys("prompt"), user("why is CI failing"))}
	c := &ChatRequest{Messages: msgs(sys("different prompt"), user("refactor the loader"))}

	if ConversationID(a) == ConversationID(b) {
		t.Error("different first user messages collided")
	}
	if ConversationID(a) == ConversationID(c) {
		t.Error("different system prompts collided")
	}
}

// TestConversationIDIsFormatAgnostic is the requirement that drove this design.
// The same conversation arriving as Anthropic, chat completions, or Gemini must
// produce one label — nothing format-specific may leak into the derivation.
func TestConversationIDIsFormatAgnostic(t *testing.T) {
	// Same IR, different provider-specific baggage on the side.
	anthropic := &ChatRequest{
		Messages:           msgs(sys("prompt"), user("hello")),
		ProviderExtensions: mustMeta(map[string]any{"anthropic_version": "2023-06-01"}),
	}
	chatCompletions := &ChatRequest{
		Messages:           msgs(sys("prompt"), user("hello")),
		ProviderExtensions: mustMeta(map[string]any{"prompt_cache_key": "some-harness-key"}),
	}
	codeAssist := &ChatRequest{
		Messages: msgs(sys("prompt"), user("hello")),
		ProviderExtensions: mustMeta(map[string]any{
			"_codeassist":    true,
			"_request_extra": map[string]any{"sessionId": "harness-session-abc"},
		}),
	}

	want := ConversationID(anthropic)
	if got := ConversationID(chatCompletions); got != want {
		t.Errorf("chat-completions label %s != anthropic label %s", got, want)
	}
	if got := ConversationID(codeAssist); got != want {
		t.Errorf("code-assist label %s != anthropic label %s — a harness sessionId leaked in", got, want)
	}
}

// TestConversationIDFieldBoundaries is the length-prefix regression test. These
// are different splits of the same concatenated bytes; without length prefixes
// they hash identically.
func TestConversationIDFieldBoundaries(t *testing.T) {
	a := &ChatRequest{Messages: msgs(sys("ab"), user("cd"))}
	b := &ChatRequest{Messages: msgs(sys("a"), user("bcd"))}

	if ConversationID(a) == ConversationID(b) {
		t.Error("field boundaries are forgeable: 'ab'+'cd' collided with 'a'+'bcd'")
	}
}

// TestConversationIDUnidentifiable pins that unidentifiable requests get "",
// never a shared key — otherwise every tool-result-only request warms one entry.
func TestConversationIDUnidentifiable(t *testing.T) {
	if got := ConversationID(nil); got != "" {
		t.Errorf("nil request = %q, want empty", got)
	}
	if got := ConversationID(&ChatRequest{}); got != "" {
		t.Errorf("empty request = %q, want empty", got)
	}
	toolOnly := &ChatRequest{Messages: msgs(Message{Role: RoleTool, Blocks: []Block{{ToolResult: &ToolResultBlock{
		ToolCallID: "1", Content: []ToolResultContentBlock{{Text: "result"}},
	}}}})}
	if got := ConversationID(toolOnly); got != "" {
		t.Errorf("tool-only request = %q, want empty", got)
	}
}

// TestConversationIDSystemOnly — a system-only prefix is a real warming target.
func TestConversationIDSystemOnly(t *testing.T) {
	if got := ConversationID(&ChatRequest{Messages: msgs(sys("prompt"))}); got == "" {
		t.Error("system-only request should be identifiable")
	}
}

// TestConversationIDMultimodalDeterministic covers the ContentParts path: an
// image-bearing first turn must hash the same way twice.
func TestConversationIDMultimodalDeterministic(t *testing.T) {
	userParts := func() Message {
		img, err := ParseRequiredJSONObject([]byte(`{"source":{"data":"abc","media_type":"image/png"}}`))
		if err != nil {
			panic(err)
		}
		return Message{Role: RoleUser, Blocks: []Block{
			{Text: &TextBlock{Text: "what is this"}},
			{Unknown: &UnknownBlock{Kind: "image", Payload: img}},
		}}
	}
	a := &ChatRequest{Messages: msgs(sys("p"), userParts())}
	b := &ChatRequest{Messages: msgs(sys("p"), userParts())}

	got := ConversationID(a)
	if got == "" {
		t.Fatal("multimodal first turn was unidentifiable")
	}
	if got != ConversationID(b) {
		t.Error("multimodal hashing is not deterministic")
	}
}

// TestKeyLength keeps both keys hand-typeable — they go into a config file and
// into `torana conversations` output.
func TestKeyLength(t *testing.T) {
	c := &ChatRequest{Messages: msgs(user("hello"))}
	for name, got := range map[string]string{"ConversationID": ConversationID(c), "CachePrefixKey": CachePrefixKey(c)} {
		if len(got) != keyBytes*2 {
			t.Errorf("%s = %q, length %d, want %d", name, got, len(got), keyBytes*2)
		}
	}
}

// --- CachePrefixKey: the cache entry ---

// TestCachePrefixKeyStableWhenPrefixStable is the core warming property. With a
// breakpoint after the system prompt, appending turns beyond it must not change
// the key — otherwise every turn looks like a new cache entry and warming can
// never target anything.
func TestCachePrefixKeyStableWhenPrefixStable(t *testing.T) {
	cached := Message{Role: RoleSystem, Blocks: []Block{
		{Text: &TextBlock{Text: "big system prompt"}},
		{CacheBreakpoint: &CacheBreakpointBlock{Marker: ephemeral()}},
	}}
	turn1 := &ChatRequest{Model: "m", Messages: msgs(cached, user("hello"))}
	turn9 := &ChatRequest{Model: "m", Messages: msgs(cached, user("hello"), asst("hi"), user("more"), asst("ok"))}

	if got, want := CachePrefixKey(turn9), CachePrefixKey(turn1); got != want {
		t.Errorf("key moved though the cached prefix did not: turn9=%s turn1=%s", got, want)
	}
}

// TestCachePrefixKeyChangesWhenPrefixRewritten is the property that makes the
// two-key split necessary. A compaction that rewrites history kills the cache
// entry, and the key must follow — warming a dead prefix is pure cost.
func TestCachePrefixKeyChangesWhenPrefixRewritten(t *testing.T) {
	before := &ChatRequest{Model: "m", Messages: msgs(
		sys("prompt"),
		user("original first message"),
		Message{Role: RoleAssistant, Blocks: []Block{
			{Text: &TextBlock{Text: "long history"}},
			{CacheBreakpoint: &CacheBreakpointBlock{Marker: ephemeral()}},
		}},
	)}
	after := &ChatRequest{Model: "m", Messages: msgs(
		sys("prompt"),
		user("[summary of earlier turns]"),
		Message{Role: RoleAssistant, Blocks: []Block{
			{Text: &TextBlock{Text: "long history"}},
			{CacheBreakpoint: &CacheBreakpointBlock{Marker: ephemeral()}},
		}},
	)}

	if CachePrefixKey(before) == CachePrefixKey(after) {
		t.Error("a rewritten prefix kept its cache key")
	}
}

// TestCachePrefixKeySplitsOnModel — provider caches never span models, so the
// same prefix on two models is two entries.
func TestCachePrefixKeySplitsOnModel(t *testing.T) {
	base := msgs(Message{Role: RoleSystem, Blocks: []Block{
		{Text: &TextBlock{Text: "p"}},
		{CacheBreakpoint: &CacheBreakpointBlock{Marker: ephemeral()}},
	}}, user("hi"))
	sonnet := &ChatRequest{Model: "claude-sonnet-4-5", Messages: base}
	opus := &ChatRequest{Model: "claude-opus-4-1", Messages: base}

	if CachePrefixKey(sonnet) == CachePrefixKey(opus) {
		t.Error("two models shared one cache key")
	}
}

// TestCachePrefixKeyIncludesToolDefs — tool definitions sit inside the cached
// prefix, so changing them invalidates the entry even when messages are equal.
func TestCachePrefixKeyIncludesToolDefs(t *testing.T) {
	base := msgs(Message{Role: RoleSystem, Blocks: []Block{
		{Text: &TextBlock{Text: "p"}},
		{CacheBreakpoint: &CacheBreakpointBlock{Marker: ephemeral()}},
	}}, user("hi"))
	a := &ChatRequest{Model: "m", Messages: base, Tools: []ToolDef{{Name: "bash"}}}
	b := &ChatRequest{Model: "m", Messages: base, Tools: []ToolDef{{Name: "bash"}, {Name: "read"}}}

	if CachePrefixKey(a) == CachePrefixKey(b) {
		t.Error("adding a tool definition kept the cache key")
	}
}

// TestCachePrefixKeyTracksBreakpointMove — moving the breakpoint changes how much
// is cached, which is a different entry.
func TestCachePrefixKeyTracksBreakpointMove(t *testing.T) {
	early := &ChatRequest{Model: "m", Messages: msgs(
		Message{Role: RoleSystem, Blocks: []Block{
			{Text: &TextBlock{Text: "p"}},
			{CacheBreakpoint: &CacheBreakpointBlock{Marker: ephemeral()}},
		}},
		user("hello"),
		asst("hi"),
	)}
	late := &ChatRequest{Model: "m", Messages: msgs(
		sys("p"),
		user("hello"),
		Message{Role: RoleAssistant, Blocks: []Block{
			{Text: &TextBlock{Text: "hi"}},
			{CacheBreakpoint: &CacheBreakpointBlock{Marker: ephemeral()}},
		}},
	)}

	if CachePrefixKey(early) == CachePrefixKey(late) {
		t.Error("breakpoint position did not affect the cache key")
	}
}

// TestCachePrefixKeyNoBreakpointHashesEverything covers automatic prefix caching
// (OpenAI, DeepSeek): with no breakpoint to bound it, the whole request is the
// prefix, so the key moves every turn. That is honest — the caller does not
// control that cache and cannot warm it.
func TestCachePrefixKeyNoBreakpointHashesEverything(t *testing.T) {
	turn1 := &ChatRequest{Model: "m", Messages: msgs(sys("p"), user("hello"))}
	turn2 := &ChatRequest{Model: "m", Messages: msgs(sys("p"), user("hello"), asst("hi"))}

	if CachePrefixKey(turn1) == CachePrefixKey(turn2) {
		t.Error("with no breakpoint, an appended turn must change the key")
	}
}

// TestCachePrefixKeyDistinctFromConversationID guards the domain separation. The
// two keys live in one namespace in config and output; one must never be
// mistaken for the other.
func TestCachePrefixKeyDistinctFromConversationID(t *testing.T) {
	c := &ChatRequest{Messages: msgs(user("hello"))}
	if ConversationID(c) == CachePrefixKey(c) {
		t.Error("conversation and cache-prefix domains are not separated")
	}
}

// TestCachePrefixKeyPreservesSignatures — a dropped thoughtSignature was a real
// production bug in the Gemini path. Signatures are part of the replayed prefix,
// so the key must notice when one changes.
func TestCachePrefixKeyPreservesSignatures(t *testing.T) {
	mk := func(sig string) *ChatRequest {
		return &ChatRequest{Model: "m", Messages: msgs(
			sys("p"),
			Message{Role: RoleAssistant, Blocks: []Block{
				{ToolUse: &ToolUseBlock{ID: "1", Name: "bash", Signature: sig}},
				{CacheBreakpoint: &CacheBreakpointBlock{Marker: ephemeral()}},
			}},
		)}
	}
	if CachePrefixKey(mk("sig-a")) == CachePrefixKey(mk("sig-b")) {
		t.Error("a changed tool-call signature kept the cache key")
	}

	// ContentSignature: a signature bound to a text part is part of the
	// replayed prefix; two requests differing only in it must split the key.
	mkContent := func(sig string) *ChatRequest {
		return &ChatRequest{Model: "m", Messages: msgs(
			sys("p"),
			Message{Role: RoleAssistant, Blocks: []Block{
				{Text: &TextBlock{Text: "answer", Signature: sig}},
				{CacheBreakpoint: &CacheBreakpointBlock{Marker: ephemeral()}},
			}},
		)}
	}
	if CachePrefixKey(mkContent("sig-a")) == CachePrefixKey(mkContent("sig-b")) {
		t.Error("a changed content signature kept the cache key")
	}

	// TrailingSignature: same invariant for the standalone final token. The
	// trailing block is FINAL by contract and a marker would close the
	// prefix BEFORE it, so this fixture carries NO marker — the whole
	// request is the automatic-cache prefix and the signature is part of it.
	mkTrailing := func(sig string) *ChatRequest {
		return &ChatRequest{Model: "m", Messages: msgs(
			sys("p"),
			Message{Role: RoleAssistant, Blocks: []Block{
				{Text: &TextBlock{Text: "answer"}},
				{TrailingSignature: &TrailingSignatureBlock{Signature: sig}},
			}},
		)}
	}
	if CachePrefixKey(mkTrailing("sig-a")) == CachePrefixKey(mkTrailing("sig-b")) {
		t.Error("a changed trailing signature kept the cache key")
	}

	// The slots are distinct even when the token bytes are identical: the
	// same token in the content slot and the trailing slot replays different
	// provider prefixes.
	if CachePrefixKey(mkContent("same")) == CachePrefixKey(mkTrailing("same")) {
		t.Error("content and trailing signatures with the same token shared a cache key")
	}
}

// TestCachePrefixKeyEmpty — nothing cacheable yields "".
func TestCachePrefixKeyEmpty(t *testing.T) {
	// "" is reserved for nil — nothing cacheable. A non-nil request is in
	// the SDK domain even with zero messages (the tool prefix is keyable),
	// so it produces a deterministic key, never a silent "".
	if got := CachePrefixKey(nil); got != "" {
		t.Errorf("nil request = %q, want empty", got)
	}
	empty := &ChatRequest{}
	if got := CachePrefixKey(empty); got == "" {
		t.Error("a non-nil empty request must produce a deterministic key")
	}
	if CachePrefixKey(empty) != CachePrefixKey(&ChatRequest{}) {
		t.Error("the empty-request key must be deterministic")
	}
}
