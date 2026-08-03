package engine

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"

	plugin_sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
)

// Review round 3 finding 1 reproductions (PB-based API): computing a cache
// key must never mutate the live request (the truncation is pure PB
// construction now), and the key must equal an INDEPENDENT reference model.

func nestedMarkerPB() *pb.ChatRequest {
	return &pb.ChatRequest{Model: "m", Messages: []*pb.Message{{Role: "user", Blocks: []*pb.RequestBlock{
		{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "a"}}},
		{Kind: &pb.RequestBlock_ToolResult{ToolResult: &pb.RequestToolResultBlock{
			ToolCallId: "c1",
			Content: []*pb.ToolResultContentBlock{
				{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "x"}}},
				{Kind: &pb.ToolResultContentBlock_CacheBreakpoint{CacheBreakpoint: &pb.ToolResultCacheBreakpoint{MarkerJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}},
				{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "y"}}},
			},
		}}},
		{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "after"}}},
	}}}}
}

func clonePBRequest(c *pb.ChatRequest) *pb.ChatRequest {
	return proto.Clone(c).(*pb.ChatRequest)
}

// TestCachePrefixKeyDoesNotMutateInput — the complete request must be
// byte/deep-equal before and after one and repeated CachePrefixKey calls.
func TestCachePrefixKeyDoesNotMutateInput(t *testing.T) {
	chat := nestedMarkerPB()
	before := clonePBRequest(chat)
	for i := 0; i < 3; i++ {
		if CachePrefixKey(chat) == "" {
			t.Fatal("key empty")
		}
	}
	if !proto.Equal(chat, before) {
		t.Fatal("CachePrefixKey mutated the live request")
	}
	tr := chat.Messages[0].Blocks[1].GetToolResult()
	if len(tr.Content) != 3 || tr.Content[2].GetText().Text != "y" {
		t.Fatalf("nested content truncated by the key computation: %+v", tr.Content)
	}
}

// TestCachePrefixKeyAliasAdversarial — two requests intentionally SHARE
// block/tool-result pointers; computing one key must not mutate the other.
func TestCachePrefixKeyAliasAdversarial(t *testing.T) {
	shared := &pb.RequestToolResultBlock{
		ToolCallId: "c1",
		Content: []*pb.ToolResultContentBlock{
			{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "x"}}},
			{Kind: &pb.ToolResultContentBlock_CacheBreakpoint{CacheBreakpoint: &pb.ToolResultCacheBreakpoint{MarkerJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}},
			{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "y"}}},
		},
	}
	a := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{{Role: "user", Blocks: []*pb.RequestBlock{
		{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "a"}}},
		{Kind: &pb.RequestBlock_ToolResult{ToolResult: shared}},
	}}}}
	b := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{{Role: "user", Blocks: []*pb.RequestBlock{
		{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "a"}}},
		{Kind: &pb.RequestBlock_ToolResult{ToolResult: shared}},
	}}}}
	if CachePrefixKey(a) == "" {
		t.Fatal("key empty")
	}
	if len(shared.Content) != 3 || shared.Content[2].GetText().Text != "y" {
		t.Fatal("computing one key mutated the shared tool-result of the other request")
	}
	if CachePrefixKey(a) != CachePrefixKey(b) {
		t.Fatal("alias requests must still produce equal keys")
	}
}

// referencePrefixKey is the INDEPENDENT reference model: it re-derives the
// expected framed hash with its OWN sha256 framing (a separate
// implementation of the length-prefix scheme), its OWN truncation, and the
// hard-coded domain tag — never the production helpers.
func referencePrefixKey(c *pb.ChatRequest, topo TopologyFacts) string {
	const domain = "torana/cache-prefix/v1"
	h := sha256.New()
	frameStr := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	frameBytes := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	frameStr(domain)
	frameStr(c.Model)

	last := lastCacheMarkerPB(c)
	toolLimit := len(c.Tools)
	if last != nil && last.kind == markerCarrierTool {
		toolLimit = last.tool + 1
	}
	for i, t := range c.Tools {
		if i >= toolLimit {
			break
		}
		frameStr("tool")
		frameStr(t.Name)
		frameStr(t.Description)
		frameBytes(t.ParametersJson)
		frameBytes(t.CacheControlJson)
	}
	frameBytes(c.ProviderExtensionsJson)
	frameBytes(c.SafetySettingsJson)

	// The typed host-only topology facts (reconstruction-changing).
	frameStr("topology")
	if topo.CodeAssist {
		frameStr("codeassist")
	} else {
		frameStr("bare")
	}
	frameStr("variant")
	frameStr(string(rune('0' + topo.OpenAIVariant)))
	frameBytes(topo.ResponsesInputLayout.Bytes())

	// The reference model mirrors production fail-closed semantics: an
	// SDK fingerprint error makes the whole key empty.
	fp := func(m *pb.Message) (string, bool) {
		s, err := plugin_sdk.RequestBlocksFingerprint(m)
		if err != nil {
			return "", false
		}
		return s, true
	}

	if last == nil {
		for _, m := range c.Messages {
			f, ok := fp(m)
			if !ok {
				return ""
			}
			frameStr("msg")
			frameStr(f)
		}
	} else if last.kind != markerCarrierTool {
		for i, m := range c.Messages {
			if i > last.msg {
				break
			}
			if i < last.msg {
				f, ok := fp(m)
				if !ok {
					return ""
				}
				frameStr("msg")
				frameStr(f)
				continue
			}
			// Independent truncation: rebuild the message from scratch.
			trunc := &pb.Message{Role: m.Role}
			for j, b := range m.Blocks {
				if j > last.block {
					break
				}
				if j == last.block && last.nested >= 0 && b.GetToolResult() != nil {
					nb := proto.Clone(b).(*pb.RequestBlock)
					tr := nb.GetToolResult()
					tr.Content = tr.Content[:last.nested+1]
					trunc.Blocks = append(trunc.Blocks, nb)
					continue
				}
				trunc.Blocks = append(trunc.Blocks, b)
			}
			f, ok := fp(trunc)
			if !ok {
				return ""
			}
			frameStr("msg")
			frameStr(f)
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:keyBytes])
}

// TestCachePrefixKeyMatchesReferenceModel — production equals the
// independent reference for top-level, nested, tool, multiple-marker, and
// no-marker cases.
func TestCachePrefixKeyMatchesReferenceModel(t *testing.T) {
	cases := map[string]*pb.ChatRequest{
		"nested marker": nestedMarkerPB(),
		"top-level marker": {Model: "m", Messages: []*pb.Message{{Role: "user", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "a"}}},
			{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}},
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "after"}}},
		}}}},
		"tool marker": {Model: "m", Messages: []*pb.Message{pbUserMsg("u")},
			Tools: []*pb.ToolDef{{Name: "read", ParametersJson: pbMarkerBytes(`{"type":"object"}`), CacheControlJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}},
		"multiple markers (message wins)": {Model: "m",
			Tools:    []*pb.ToolDef{{Name: "read", ParametersJson: pbMarkerBytes(`{}`), CacheControlJson: pbMarkerBytes(`{"type":"ephemeral"}`)}},
			Messages: []*pb.Message{pbUserMsg("u"), pbUserMsg("v")}},
		"no marker": {Model: "m", Messages: []*pb.Message{pbUserMsg("a"), pbUserMsg("b")}},
		"nested marker in second message": {Model: "m", Messages: []*pb.Message{
			pbUserMsg("first"),
			{Role: "user", Blocks: []*pb.RequestBlock{
				{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "a"}}},
				{Kind: &pb.RequestBlock_ToolResult{ToolResult: &pb.RequestToolResultBlock{
					ToolCallId: "c1",
					Content: []*pb.ToolResultContentBlock{
						{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "x"}}},
						{Kind: &pb.ToolResultContentBlock_CacheBreakpoint{CacheBreakpoint: &pb.ToolResultCacheBreakpoint{MarkerJson: pbMarkerBytes(`{"type":"ephemeral"}`)}}},
						{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "y"}}},
					},
				}}},
			}},
			pbUserMsg("later"),
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := CachePrefixKey(c)
			want := referencePrefixKey(c, TopologyFacts{})
			if got != want {
				t.Fatalf("production key %q != reference key %q", got, want)
			}
			if got == "" {
				t.Fatal("key unexpectedly empty")
			}
		})
	}
}

// TestCachePrefixKeyTopologySensitive — the typed host-only topology facts
// change the key: the code-assist flag, the OpenAI variant, and the
// Responses layout bytes each flip it (different reconstruction => a key
// that aliased them would target the wrong provider cache entry).
func TestCachePrefixKeyTopologySensitive(t *testing.T) {
	base := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{pbUserMsg("u")}}
	key := func(topo TopologyFacts) string { return CachePrefixKeyTopology(base, topo) }
	ref := func(topo TopologyFacts) string { return referencePrefixKey(base, topo) }

	plain := key(TopologyFacts{})
	if plain == "" || plain != ref(TopologyFacts{}) {
		t.Fatalf("plain key %q vs reference %q", plain, ref(TopologyFacts{}))
	}
	codeAssist := key(TopologyFacts{CodeAssist: true})
	if codeAssist == plain || codeAssist != ref(TopologyFacts{CodeAssist: true}) {
		t.Fatal("code-assist flag must flip the key and match the reference")
	}
	responses := key(TopologyFacts{OpenAIVariant: OpenAIResponses})
	if responses == plain || responses != ref(TopologyFacts{OpenAIVariant: OpenAIResponses}) {
		t.Fatal("openai variant must flip the key and match the reference")
	}
	layout, err := ParseOptionalJSONArray([]byte(`[{"type":"message","content":"x"},{"type":"reasoning"}]`))
	if err != nil {
		t.Fatal(err)
	}
	withLayout := key(TopologyFacts{ResponsesInputLayout: layout})
	if withLayout == plain || withLayout != ref(TopologyFacts{ResponsesInputLayout: layout}) {
		t.Fatal("responses layout must flip the key and match the reference")
	}
	// Combined facts are order-independent in effect: any distinct
	// combination is a distinct key.
	combo := key(TopologyFacts{CodeAssist: true, OpenAIVariant: OpenAIResponses})
	if combo == plain || combo == codeAssist || combo == responses {
		t.Fatal("combined facts must produce a distinct key")
	}
}
