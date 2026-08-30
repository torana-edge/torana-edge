package engine

import (
	"encoding/json"
	"google.golang.org/protobuf/proto"
	"testing"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
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
		CodeAssist: true, // typed host-only topology (never in the ABI)
		Messages:   msgs(sys("prompt"), user("hello")),
		ProviderExtensions: mustMeta(map[string]any{
			// The deliberate 4b envelope: outer extras at top, inner extras
			// under `request`.
			"request": map[string]any{"sessionId": "harness-session-abc"},
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
	pbC := &pb.ChatRequest{Model: c.Model, Messages: []*pb.Message{{Role: "user", Blocks: []*pb.RequestBlock{{
		Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "hello"}},
	}}}}}
	for name, got := range map[string]string{"ConversationID": ConversationID(c), "CachePrefixKey": CachePrefixKey(pbC)} {
		if len(got) != keyBytes*2 {
			t.Errorf("%s = %q, length %d, want %d", name, got, len(got), keyBytes*2)
		}
	}
}

// --- CachePrefixKey: the cache entry (PB API) ---

func pbSys(text string) *pb.Message {
	return &pb.Message{Role: "system", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: text}}}}}
}

func pbAsst(text string) *pb.Message {
	return &pb.Message{Role: "assistant", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: text}}}}}
}

func pbCachedSys(text string) *pb.Message {
	return &pb.Message{Role: "system", Blocks: []*pb.RequestBlock{
		{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: text}}},
		{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}},
	}}
}

// TestCachePrefixKeyStableWhenPrefixStable is the core warming property. With a
// breakpoint after the system prompt, appending turns beyond it must not change
// the key — otherwise every turn looks like a new cache entry and warming can
// never target anything.
func TestCachePrefixKeyStableWhenPrefixStable(t *testing.T) {
	cached := pbCachedSys("big system prompt")
	turn1 := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{cached, pbUserMsg("hello")}}
	turn9 := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{cached, pbUserMsg("hello"), pbAsst("hi"), pbUserMsg("more"), pbAsst("ok")}}

	if got, want := CachePrefixKey(turn9), CachePrefixKey(turn1); got != want {
		t.Errorf("key moved though the cached prefix did not: turn9=%s turn1=%s", got, want)
	}
}

// TestCachePrefixKeyChangesWhenPrefixRewritten is the property that makes the
// two-key split necessary. A compaction that rewrites history kills the cache
// entry, and the key must follow — warming a dead prefix is pure cost.
func TestCachePrefixKeyChangesWhenPrefixRewritten(t *testing.T) {
	before := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{
		pbSys("prompt"),
		pbUserMsg("original first message"),
		pbCachedSys("long history"),
	}}
	after := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{
		pbSys("prompt"),
		pbUserMsg("[summary of earlier turns]"),
		pbCachedSys("long history"),
	}}

	if CachePrefixKey(before) == CachePrefixKey(after) {
		t.Error("a rewritten prefix kept its cache key")
	}
}

// TestCachePrefixKeySplitsOnModel — provider caches never span models, so the
// same prefix on two models is two entries.
func TestCachePrefixKeySplitsOnModel(t *testing.T) {
	base := []*pb.Message{pbCachedSys("p"), pbUserMsg("hi")}
	sonnet := &pb.ChatRequest{Model: "claude-sonnet-4-5", Messages: base}
	opus := &pb.ChatRequest{Model: "claude-opus-4-1", Messages: base}

	if CachePrefixKey(sonnet) == CachePrefixKey(opus) {
		t.Error("two models shared one cache key")
	}
}

// TestCachePrefixKeyIncludesToolDefs — tool definitions sit inside the cached
// prefix, so changing them invalidates the entry even when messages are equal.
func TestCachePrefixKeyIncludesToolDefs(t *testing.T) {
	base := []*pb.Message{pbCachedSys("p"), pbUserMsg("hi")}
	a := &pb.ChatRequest{Model: "m", Messages: base, Tools: []*pb.ToolDef{{Name: "bash", ParametersJson: pbMarkerBytes(`{}`)}}}
	b := &pb.ChatRequest{Model: "m", Messages: base, Tools: []*pb.ToolDef{{Name: "bash", ParametersJson: pbMarkerBytes(`{}`)}, {Name: "read", ParametersJson: pbMarkerBytes(`{}`)}}}

	if CachePrefixKey(a) == CachePrefixKey(b) {
		t.Error("adding a tool definition kept the cache key")
	}
}

// TestCachePrefixKeyTracksBreakpointMove — moving the breakpoint changes how much
// is cached, which is a different entry.
func TestCachePrefixKeyTracksBreakpointMove(t *testing.T) {
	early := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{
		pbCachedSys("p"),
		pbUserMsg("hello"),
		pbAsst("hi"),
	}}
	late := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{
		pbSys("p"),
		pbUserMsg("hello"),
		{Role: "assistant", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "hi"}}},
			{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}},
		}},
	}}

	if CachePrefixKey(early) == CachePrefixKey(late) {
		t.Error("breakpoint position did not affect the cache key")
	}
}

// TestCachePrefixKeyNoBreakpointHashesEverything covers automatic prefix caching
// (OpenAI, DeepSeek): with no breakpoint to bound it, the whole request is the
// prefix, so the key moves every turn. That is honest — the caller does not
// control that cache and cannot warm it.
func TestCachePrefixKeyNoBreakpointHashesEverything(t *testing.T) {
	turn1 := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{pbSys("p"), pbUserMsg("hello")}}
	turn2 := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{pbSys("p"), pbUserMsg("hello"), pbAsst("hi")}}

	if CachePrefixKey(turn1) == CachePrefixKey(turn2) {
		t.Error("with no breakpoint, an appended turn must change the key")
	}
}

// TestCachePrefixKeyDistinctFromConversationID guards the domain separation. The
// two keys live in one namespace in config and output; one must never be
// mistaken for the other.
func TestCachePrefixKeyDistinctFromConversationID(t *testing.T) {
	c := &ChatRequest{Messages: msgs(user("hello"))}
	if ConversationID(c) == CachePrefixKey(&pb.ChatRequest{Model: c.Model, Messages: []*pb.Message{pbUserMsg("hello")}}) {
		t.Error("conversation and cache-prefix domains are not separated")
	}
}

// TestCachePrefixKeyPreservesSignatures — a dropped thoughtSignature was a real
// production bug in the Gemini path. Signatures are part of the replayed prefix,
// so the key must notice when one changes. The tool-call and content-bound
// signatures ride blocks BEFORE a marker (in-prefix); the trailing signature is
// FINAL by contract, so its fixture carries NO marker — the whole request is
// the automatic-cache prefix.
func TestCachePrefixKeyPreservesSignatures(t *testing.T) {
	mk := func(sig string) *pb.ChatRequest {
		return &pb.ChatRequest{Model: "m", Messages: []*pb.Message{
			pbSys("p"),
			{Role: "assistant", Blocks: []*pb.RequestBlock{
				{Kind: &pb.RequestBlock_ToolUse{ToolUse: &pb.RequestToolUseBlock{Id: "1", Name: "bash", Signature: sig, ArgumentsJson: pbMarkerBytes(`{}`)}}},
				{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}},
			}},
		}}
	}
	if CachePrefixKey(mk("sig-a")) == CachePrefixKey(mk("sig-b")) {
		t.Error("a changed tool-call signature kept the cache key")
	}

	// ContentSignature: a signature bound to a text part is part of the
	// replayed prefix; two requests differing only in it must split the key.
	mkContent := func(sig string) *pb.ChatRequest {
		return &pb.ChatRequest{Model: "m", Messages: []*pb.Message{
			pbSys("p"),
			{Role: "assistant", Blocks: []*pb.RequestBlock{
				{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "answer", Signature: sig}}},
				{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}},
			}},
		}}
	}
	if CachePrefixKey(mkContent("sig-a")) == CachePrefixKey(mkContent("sig-b")) {
		t.Error("a changed content signature kept the cache key")
	}

	// TrailingSignature: same invariant for the standalone final token. The
	// trailing block is FINAL by contract and a marker would close the
	// prefix BEFORE it, so this fixture carries NO marker — the whole
	// request is the automatic-cache prefix and the signature is part of it.
	mkTrailing := func(sig string) *pb.ChatRequest {
		return &pb.ChatRequest{Model: "m", Messages: []*pb.Message{
			pbSys("p"),
			{Role: "assistant", Blocks: []*pb.RequestBlock{
				{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "answer"}}},
				{Kind: &pb.RequestBlock_TrailingSignature{TrailingSignature: &pb.RequestTrailingSignatureBlock{Signature: sig}}},
			}},
		}}
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

// TestCachePrefixKeyEmpty — the key op is self-gated by the SDK's full-domain
// validator: "" for nil and for out-of-domain requests; a non-nil in-domain
// request (even with zero messages) gets a deterministic key.
func TestCachePrefixKeyEmpty(t *testing.T) {
	if got := CachePrefixKey(nil); got != "" {
		t.Errorf("nil request = %q, want empty", got)
	}
	empty := &pb.ChatRequest{}
	if got := CachePrefixKey(empty); got == "" {
		t.Error("a non-nil in-domain empty request must produce a deterministic key")
	}
	if CachePrefixKey(empty) != CachePrefixKey(&pb.ChatRequest{}) {
		t.Error("the empty-request key must be deterministic")
	}
	// Out-of-domain: a message without blocks gets no key.
	invalid := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{{Role: "user"}}}
	if got := CachePrefixKey(invalid); got != "" {
		t.Errorf("an SDK-invalid request got a key %q; want the fail-safe empty key", got)
	}
}

// prefixKeyBase is a VALID request with a top-level breakpoint marker (the
// prefix truncation path) carrying every observable field.
func prefixKeyBase() *pb.ChatRequest {
	return &pb.ChatRequest{
		Model:         "m",
		MaxTokens:     proto.Int32(64),
		Temperature:   proto.Float64(0.5),
		TopP:          proto.Float64(0.9),
		StopSequences: []string{"END"},
		Tools: []*pb.ToolDef{{
			Name:           "read",
			Description:    "d",
			ParametersJson: []byte(`{"type":"object"}`),
		}},
		ProviderExtensionsJson: []byte(`{"custom":{"b":1,"a":2}}`),
		SafetySettingsJson:     []byte(`[{"category":"HARM_CATEGORY_X"}]`),
		Messages: []*pb.Message{
			{Role: "user", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "hi"}}}}},
			{Role: "user", Blocks: []*pb.RequestBlock{
				{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "more"}}},
				{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: []byte(`{"type":"ephemeral"}`)}}},
			}},
		},
	}
}

// TestCachePrefixKeyObservableFieldSensitivity — the approved sensitivity
// contract survives the reconciliation: every observable field (model,
// tools, messages, provider extensions, safety settings, generation
// params) moves the key, while stream and torana_meta_json (excluded by
// the SDK projection) must NOT.
func TestCachePrefixKeyObservableFieldSensitivity(t *testing.T) {
	included := map[string]func(*pb.ChatRequest){
		"model":                    func(r *pb.ChatRequest) { r.Model = "m2" },
		"tools":                    func(r *pb.ChatRequest) { r.Tools[0].Description = "d2" },
		"messages":                 func(r *pb.ChatRequest) { r.Messages[0].Blocks[0].GetText().Text = "changed" },
		"provider_extensions_json": func(r *pb.ChatRequest) { r.ProviderExtensionsJson = []byte(`{"custom":{"a":2,"b":1}}`) },
		"safety_settings_json":     func(r *pb.ChatRequest) { r.SafetySettingsJson = []byte(`[]`) },
		"max_tokens":               func(r *pb.ChatRequest) { r.MaxTokens = proto.Int32(128) },
		"temperature":              func(r *pb.ChatRequest) { r.Temperature = proto.Float64(0.25) },
		"top_p":                    func(r *pb.ChatRequest) { r.TopP = proto.Float64(0.1) },
		"stop_sequences":           func(r *pb.ChatRequest) { r.StopSequences = []string{"END", "STOP"} },
	}
	excluded := map[string]func(*pb.ChatRequest){
		"stream":           func(r *pb.ChatRequest) { r.Stream = true },
		"torana_meta_json": func(r *pb.ChatRequest) { r.ToranaMetaJson = []byte(`{"_provider":"x"}`) },
	}
	for name, mutate := range included {
		t.Run("included/"+name, func(t *testing.T) {
			base := prefixKeyBase()
			before := CachePrefixKey(base)
			if before == "" {
				t.Fatal("fixture key empty; vacuous")
			}
			mutate(base)
			if got := CachePrefixKey(base); got == before {
				t.Errorf("observable field %s did not move the key", name)
			}
		})
	}
	for name, mutate := range excluded {
		t.Run("excluded/"+name, func(t *testing.T) {
			base := prefixKeyBase()
			before := CachePrefixKey(base)
			mutate(base)
			if got := CachePrefixKey(base); got != before {
				t.Errorf("excluded field %s moved the key", name)
			}
		})
	}
}

// TestCachePrefixKeyTopologyOnlyDivergence — identical observable prefixes
// with different host-only topology facts key differently; identical
// prefixes with identical topology key identically. The topology layer is
// the ENTIRE divergence between the Edge key and the observable prefix.
func TestCachePrefixKeyTopologyOnlyDivergence(t *testing.T) {
	base := prefixKeyBase()
	prefix := CachePrefixKey(base)
	if prefix == "" {
		t.Fatal("fixture key empty; vacuous")
	}

	variants := []struct {
		name string
		topo TopologyFacts
	}{
		{"bare/chat", TopologyFacts{CodeAssist: false, OpenAIVariant: OpenAIChat, ResponsesInputLayout: OptionalJSONArray{}}},
		{"codeassist", TopologyFacts{CodeAssist: true, OpenAIVariant: OpenAIChat, ResponsesInputLayout: OptionalJSONArray{}}},
		{"responses-variant", TopologyFacts{CodeAssist: false, OpenAIVariant: OpenAIResponses, ResponsesInputLayout: OptionalJSONArray{}}},
		{"responses-layout", TopologyFacts{CodeAssist: false, OpenAIVariant: OpenAIChat, ResponsesInputLayout: mustArray(`[{"type":"message"}]`)}},
	}
	seen := map[string]string{}
	for _, v := range variants {
		// Identical PB + identical topology ⇒ identical key.
		if a, b := CachePrefixKeyTopology(proto.Clone(base).(*pb.ChatRequest), v.topo), CachePrefixKeyTopology(proto.Clone(base).(*pb.ChatRequest), v.topo); a != b || a == "" {
			t.Fatalf("variant %s: identical topology must key identically (got %q/%q)", v.name, a, b)
		}
		// Identical PB + different topology ⇒ different key.
		k := CachePrefixKeyTopology(proto.Clone(base).(*pb.ChatRequest), v.topo)
		for other, otherK := range seen {
			if k == otherK {
				t.Fatalf("variant %s collides with %s: the topology layer must diverge the key", v.name, other)
			}
		}
		seen[v.name] = k
	}
	// The plain CachePrefixKey equals the zero-topology framing.
	if got := CachePrefixKey(base); got != CachePrefixKeyTopology(base, TopologyFacts{}) {
		t.Fatal("CachePrefixKey must equal the zero-topology framing")
	}
}

// mustArray parses a JSON array into the typed OptionalJSONArray.
func mustArray(raw string) OptionalJSONArray {
	arr, err := ParseOptionalJSONArray([]byte(raw))
	if err != nil {
		panic(err)
	}
	return arr
}
