package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// Mutation suite for the production write-grant verifier (writegrant.go).
//
// This is enforcement, and the plugin author is the threat model: the verifier
// decides whether a plugin exceeded its grants. A method that merely usually
// notices a change is not a candidate. An earlier prototype folded a per-role
// accumulator and concatenated fields unframed, which missed a cross-role
// reorder entirely and collided across field boundaries — both reproduced in
// the tests below, both disqualifying.
//
// Production change detection is fingerprintRequestSections (writegrant.go):
// SHA-256 over length-framed fields with the message index folded in,
// collision-resistant and reorder-sensitive. Each message is first hashed as
// its own typed record (fingerprintMessage) — field tags + presence + an
// explicit tool-call count and per-call records — so a tool call can never be
// confused with a message or its neighbour's fields (the round-1 structural
// ambiguity is reproduced below). The tests here exercise that production
// fingerprint. compareSections — exact structural comparison, no hashing,
// which cannot collide because it never summarises — is kept below as the
// mutation suite's oracle: a mutation must be caught by BOTH the exact
// comparison and the fingerprint, so a regression in either fails the suite.

// changedSections names the grantable sections a plugin's output differs in.
type changedSections struct {
	messages map[string]bool // role → changed
	tools    bool
	model    bool
	params   bool
	// cacheControl marks a change to Message.cache_control_json or
	// ToolDef.cache_control_json, governed by ir.cache_control.write alone.
	cacheControl bool
	// hostOwned marks a change to a field no grant covers, because the host
	// owns it. torana_meta_json is the only one: the host writes _provider and
	// friends into it, and under v2 verdicts are host calls rather than keys in
	// it, so a plugin has no legitimate reason to alter it at all. There is no
	// grant that permits this — it is a violation outright.
	hostOwned bool
}

func (c changedSections) any() bool {
	return len(c.messages) > 0 || c.tools || c.model || c.params || c.cacheControl || c.hostOwned
}

// compareSections diffs a plugin's output against the previously accepted
// request, exactly.
//
// Messages are compared positionally. Any index where the two differ marks the
// roles on BOTH sides as changed — a reorder or a replacement involves the role
// that left the slot and the role that took it, and requiring both is the
// conservative reading. A length difference marks every role in the surplus.
func compareSections(accepted, out *pb.ChatRequest) changedSections {
	c := changedSections{messages: map[string]bool{}}

	n := len(accepted.Messages)
	if len(out.Messages) > n {
		n = len(out.Messages)
	}
	for i := 0; i < n; i++ {
		var a, b *pb.Message
		if i < len(accepted.Messages) {
			a = accepted.Messages[i]
		}
		if i < len(out.Messages) {
			b = out.Messages[i]
		}
		switch {
		case a == nil:
			c.messages[b.Role] = true
		case b == nil:
			c.messages[a.Role] = true
		case !sameMessageWithoutMarker(a, b):
			c.messages[a.Role] = true
			c.messages[b.Role] = true
		}
	}

	if len(accepted.Tools) != len(out.Tools) {
		c.tools = true
	} else {
		for i := range accepted.Tools {
			if !sameToolWithoutMarker(accepted.Tools[i], out.Tools[i]) {
				c.tools = true
				break
			}
		}
	}

	c.model = accepted.Model != out.Model
	c.params = !sameParams(accepted, out)
	c.hostOwned = !bytes.Equal(accepted.ToranaMetaJson, out.ToranaMetaJson)
	c.cacheControl = cacheControlChanged(accepted, out)
	return c
}

// cacheControlChanged compares the cache breakpoint markers positionally,
// mirroring the production fingerprint section: ONLY marker-carrying messages
// and tools participate (a marker moved between any two positions counts as
// changed even when its bytes are unchanged; marker-less structural changes
// do not).
func cacheControlChanged(accepted, out *pb.ChatRequest) bool {
	n := len(accepted.Messages)
	if len(out.Messages) > n {
		n = len(out.Messages)
	}
	for i := 0; i < n; i++ {
		var a, b []byte
		if i < len(accepted.Messages) {
			a = cacheMarkers(accepted.Messages[i])
		}
		if i < len(out.Messages) {
			b = cacheMarkers(out.Messages[i])
		}
		if !bytes.Equal(a, b) {
			return true
		}
	}
	nt := len(accepted.Tools)
	if len(out.Tools) > nt {
		nt = len(out.Tools)
	}
	for i := 0; i < nt; i++ {
		var a, b []byte
		if i < len(accepted.Tools) && len(accepted.Tools[i].CacheControlJson) > 0 {
			a = accepted.Tools[i].CacheControlJson
		}
		if i < len(out.Tools) && len(out.Tools[i].CacheControlJson) > 0 {
			b = out.Tools[i].CacheControlJson
		}
		if !bytes.Equal(a, b) {
			return true
		}
	}
	return false
}

// sameMessageWithoutMarker compares two messages EXCLUDING
// cache_control_json: markers are governed by ir.cache_control.write as a
// section of their own (cacheControlChanged), never by the role sections —
// the oracle must agree with the production fingerprint, or a marker-only
// mutation would look like a role change to the exact comparison.
func sameMessageWithoutMarker(a, b *pb.Message) bool {
	ca := stripCacheBlocks(a)
	cb := stripCacheBlocks(b)
	return proto.Equal(ca, cb)
}

// sameToolWithoutMarker compares two tool definitions EXCLUDING
// cache_control_json, for the same reason.
func stripCacheBlocks(m *pb.Message) *pb.Message {
	ca := proto.Clone(m).(*pb.Message)
	var kept []*pb.RequestBlock
	for _, b := range ca.Blocks {
		if b.GetCacheBreakpoint() != nil {
			continue
		}
		if tr := b.GetToolResult(); tr != nil {
			tr.Content = nestedWithoutCache(tr.Content)
		}
		kept = append(kept, b)
	}
	ca.Blocks = kept
	return ca
}

// cacheMarkers encodes a message's cache carriers POSITIONALLY, mirroring
// the production cache section: each marker frame carries the outer block
// index, the carrier tag (1 top-level, 2 nested), the nested index (-1 for
// top-level), and the marker bytes. A marker moved between any two positions
// changes the encoding even when its bytes are unchanged.
func cacheMarkers(m *pb.Message) []byte {
	var out []byte
	for bi, b := range m.Blocks {
		if cb := b.GetCacheBreakpoint(); cb != nil {
			out = append(out, markerFrame(bi, 1, -1, cb.MarkerJson)...)
		}
		if tr := b.GetToolResult(); tr != nil {
			for ci, c := range tr.Content {
				if cb := c.GetCacheBreakpoint(); cb != nil {
					out = append(out, markerFrame(bi, 2, ci, cb.MarkerJson)...)
				}
			}
		}
	}
	return out
}

func markerFrame(blockIdx int, carrier int, nestedIdx int, bytes []byte) []byte {
	var f []byte
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(blockIdx))
	f = append(f, b[:]...)
	binary.LittleEndian.PutUint64(b[:], uint64(carrier))
	f = append(f, b[:]...)
	binary.LittleEndian.PutUint64(b[:], uint64(nestedIdx))
	f = append(f, b[:]...)
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(bytes)))
	f = append(f, n[:]...)
	f = append(f, bytes...)
	return f
}

func sameToolWithoutMarker(a, b *pb.ToolDef) bool {
	ca := proto.Clone(a).(*pb.ToolDef)
	cb := proto.Clone(b).(*pb.ToolDef)
	ca.CacheControlJson, cb.CacheControlJson = nil, nil
	return proto.Equal(ca, cb)
}

func sameParams(a, b *pb.ChatRequest) bool {
	if a.Stream != b.Stream ||
		!sameInt32Ptr(a.MaxTokens, b.MaxTokens) ||
		!sameFloatPtr(a.Temperature, b.Temperature) ||
		!sameFloatPtr(a.TopP, b.TopP) ||
		len(a.StopSequences) != len(b.StopSequences) ||
		!bytes.Equal(a.ProviderExtensionsJson, b.ProviderExtensionsJson) ||
		!bytes.Equal(a.SafetySettingsJson, b.SafetySettingsJson) {
		return false
	}
	for i := range a.StopSequences {
		if a.StopSequences[i] != b.StopSequences[i] {
			return false
		}
	}
	return true
}

func sameInt32Ptr(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// sameFloatPtr compares bit patterns, not values. `==` reports -0.0 and +0.0 as
// equal, so a plugin could flip the sign bit undetected, and reports NaN as
// unequal to itself, so a NaN parameter would be flagged as changed on every
// request it survives.
func sameFloatPtr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return math.Float64bits(*a) == math.Float64bits(*b)
}

// --- correctness ------------------------------------------------------------

// mutation describes a change a plugin could make that the verifier must catch.
type mutation struct {
	name string
	// wantRoles are the message roles the change must be attributed to. Empty
	// means the change is not in the messages section.
	wantRoles []string
	apply     func(*pb.ChatRequest)
}

func baseRequest() *pb.ChatRequest {
	temp := 0.0
	maxTok := int32(1024)
	return &pb.ChatRequest{
		Model:       "claude-sonnet-4",
		Temperature: &temp,
		MaxTokens:   &maxTok,
		Messages: []*pb.Message{
			{Role: "system", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "you are helpful"}}}}},
			{Role: "user", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "A"}}}}},
			{Role: "assistant", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_ToolUse{ToolUse: &pb.RequestToolUseBlock{Id: "c1", Name: "read", ArgumentsJson: []byte(`{"p":1}`)}}}}},
			{Role: "tool", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_ToolResult{ToolResult: &pb.RequestToolResultBlock{ToolCallId: "c1", ToolName: "read", Content: []*pb.ToolResultContentBlock{{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "B"}}}}}}}}},
		},
		Tools: []*pb.ToolDef{{Name: "read", Description: "read a file", ParametersJson: []byte(`{"type":"object"}`)}},
	}
}

func mutations() []mutation {
	return []mutation{
		{
			// The case the first prototype missed entirely.
			name:      "cross-role reorder",
			wantRoles: []string{"user", "tool"},
			apply: func(r *pb.ChatRequest) {
				r.Messages[1], r.Messages[3] = r.Messages[3], r.Messages[1]
			},
		},
		{
			name:      "same-role content edit",
			wantRoles: []string{"tool"},
			apply: func(r *pb.ChatRequest) {
				r.Messages[3].Blocks[0].GetToolResult().Content[0].Kind = &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "B'"}}
			},
		},
		{
			// Unframed concatenation makes these two indistinguishable.
			name:      "field boundary shift",
			wantRoles: []string{"tool"},
			apply: func(r *pb.ChatRequest) {
				r.Messages[3].Blocks[0].GetToolResult().ToolName = "read" + r.Messages[3].Blocks[0].GetToolResult().Content[0].GetText().Text
				r.Messages[3].Blocks[0].GetToolResult().Content[0].GetText().Text = ""
			},
		},
		{
			name:      "message inserted",
			wantRoles: []string{"user"},
			apply: func(r *pb.ChatRequest) {
				r.Messages = append(r.Messages, &pb.Message{Role: "user", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "C"}}}}})
			},
		},
		{
			name:      "message deleted",
			wantRoles: []string{"tool"},
			apply:     func(r *pb.ChatRequest) { r.Messages = r.Messages[:3] },
		},
		{
			name:      "tool call arguments rewritten",
			wantRoles: []string{"assistant"},
			apply:     func(r *pb.ChatRequest) { r.Messages[2].Blocks[0].GetToolUse().ArgumentsJson = []byte(`{"p":2}`) },
		},
		{
			name:  "tool schema rewritten",
			apply: func(r *pb.ChatRequest) { r.Tools[0].ParametersJson = []byte(`{"type":"array"}`) },
		},
		{
			name:  "model swapped",
			apply: func(r *pb.ChatRequest) { r.Model = "claude-opus-4" },
		},
		{
			name: "temperature changed",
			apply: func(r *pb.ChatRequest) {
				v := 1.0
				r.Temperature = &v
			},
		},
	}
}

// Every mutation must be detected by exact comparison, and attributed to the
// right roles.
func TestCompareSectionsDetectsEveryMutation(t *testing.T) {
	for _, m := range mutations() {
		t.Run(m.name, func(t *testing.T) {
			accepted := baseRequest()
			out := baseRequest()
			m.apply(out)

			c := compareSections(accepted, out)
			if !c.any() {
				t.Fatal("mutation not detected — a plugin could make this change without the grant")
			}
			for _, role := range m.wantRoles {
				if !c.messages[role] {
					t.Errorf("change not attributed to role %q; got %v", role, c.messages)
				}
			}
		})
	}
}

// The safe fingerprint must detect the same set. This is the property the
// first prototype failed.
func TestSafeFingerprintDetectsEveryMutation(t *testing.T) {
	for _, m := range mutations() {
		t.Run(m.name, func(t *testing.T) {
			accepted := fingerprintRequestSections(baseRequest())
			out := baseRequest()
			m.apply(out)

			if accepted.equal(fingerprintRequestSections(out)) {
				t.Fatal("mutation invisible to the fingerprint — not safe for enforcement")
			}
		})
	}
}

// An untouched request must compare equal under both methods, or every plugin
// would be rejected.
func TestUnmodifiedRequestIsUnchanged(t *testing.T) {
	if c := compareSections(baseRequest(), baseRequest()); c.any() {
		t.Fatalf("unmodified request reported as changed: %+v", c)
	}
	if !fingerprintRequestSections(baseRequest()).equal(fingerprintRequestSections(baseRequest())) {
		t.Fatal("unmodified request produced different fingerprints")
	}
}

// --- field inventory --------------------------------------------------------
//
// The inventory tables themselves (chatRequestFieldSections and friends) live
// in writegrant.go next to the verifier; the tests below are what make them
// enforceable.

// Every protobuf field must be assigned to a grant section or explicitly
// host-owned. An unassigned field is a field a plugin could change with no
// grant and no detection.
func TestEveryProtoFieldHasAGrantSection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		msg      proto.Message
		sections map[string]string
	}{
		{"ChatRequest", &pb.ChatRequest{}, chatRequestFieldSections},
		{"Message", &pb.Message{}, messageFieldSections},
		{"RequestBlock", &pb.RequestBlock{}, requestBlockFieldSections},
		{"RequestTextBlock", &pb.RequestTextBlock{}, requestTextBlockFieldSections},
		{"RequestThinkingBlock", &pb.RequestThinkingBlock{}, requestThinkingBlockFieldSections},
		{"RequestRedactedThinkingBlock", &pb.RequestRedactedThinkingBlock{}, requestRedactedThinkingBlockFieldSections},
		{"RequestToolUseBlock", &pb.RequestToolUseBlock{}, requestToolUseBlockFieldSections},
		{"RequestToolResultBlock", &pb.RequestToolResultBlock{}, requestToolResultBlockFieldSections},
		{"ToolResultContentBlock", &pb.ToolResultContentBlock{}, toolResultContentBlockFieldSections},
		{"ToolResultTextBlock", &pb.ToolResultTextBlock{}, toolResultTextBlockFieldSections},
		{"ToolResultUnknownBlock", &pb.ToolResultUnknownBlock{}, toolResultUnknownBlockFieldSections},
		{"RequestUnknownBlock", &pb.RequestUnknownBlock{}, requestUnknownBlockFieldSections},
		{"RequestTrailingSignatureBlock", &pb.RequestTrailingSignatureBlock{}, requestTrailingSignatureBlockFieldSections},
		{"RequestCacheBreakpoint", &pb.RequestCacheBreakpoint{}, requestCacheBreakpointFieldSections},
		{"ToolResultCacheBreakpoint", &pb.ToolResultCacheBreakpoint{}, toolResultCacheBreakpointFieldSections},
		{"ToolCall", &pb.ToolCall{}, toolCallFieldSections},
		{"ToolDef", &pb.ToolDef{}, toolDefFieldSections},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := tc.msg.ProtoReflect().Descriptor().Fields()
			seen := map[string]bool{}
			for i := 0; i < fields.Len(); i++ {
				name := string(fields.Get(i).Name())
				seen[name] = true
				if _, ok := tc.sections[name]; !ok {
					t.Errorf("%s.%s belongs to no grant section — a plugin could change it "+
						"with no grant and no detection. Assign it, or mark it %s",
						tc.name, name, hostOwnedField)
				}
			}
			for name := range tc.sections {
				if !seen[name] {
					t.Errorf("%s.%s is mapped to a section but no longer exists in the proto",
						tc.name, name)
				}
			}
		})
	}
}

// Each governed field must actually be detected as changed — including nested
// ones. This is what turns the inventory above from documentation into
// enforcement.
//
// Walking only ChatRequest's own fields was not enough: Message, ToolCall and
// ToolDef were inventoried but never mutated, so a nested field could be
// assigned a grant section and still be missing from the fingerprint without
// failing anything. The nested cases are exactly where the earlier prototypes
// went wrong.
func TestEveryGovernedFieldIsDetected(t *testing.T) {
	type target struct {
		name string
		// pick returns the nested message to mutate within a fresh request.
		pick     func(*pb.ChatRequest) proto.Message
		sections map[string]string
	}
	targets := []target{
		{"ChatRequest", func(r *pb.ChatRequest) proto.Message { return r }, chatRequestFieldSections},
		{"Message", func(r *pb.ChatRequest) proto.Message { return r.Messages[3] }, messageFieldSections},
		{"RequestBlock", func(r *pb.ChatRequest) proto.Message { return r.Messages[2].Blocks[0] }, requestBlockFieldSections},
		{"RequestTextBlock", func(r *pb.ChatRequest) proto.Message { return r.Messages[1].Blocks[0].GetText() }, requestTextBlockFieldSections},
		{"RequestToolUseBlock", func(r *pb.ChatRequest) proto.Message { return r.Messages[2].Blocks[0].GetToolUse() }, requestToolUseBlockFieldSections},
		{"ToolDef", func(r *pb.ChatRequest) proto.Message { return r.Tools[0] }, toolDefFieldSections},
	}

	for _, tg := range targets {
		fields := tg.pick(baseRequest()).ProtoReflect().Descriptor().Fields()
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			name := string(fd.Name())
			if tg.sections[name] == hostOwnedField {
				// Host-owned fields are detected as a violation rather than a
				// grantable change; covered by their own test below.
				continue
			}
			t.Run(tg.name+"/"+name, func(t *testing.T) {
				accepted := baseRequest()
				out := baseRequest()
				mutateField(t, tg.pick(out), fd)

				if !compareSections(accepted, out).any() {
					t.Error("exact comparison did not detect a change to this field")
				}
				if fingerprintRequestSections(accepted).equal(fingerprintRequestSections(out)) {
					t.Error("fingerprint did not detect a change to this field")
				}
			})
		}
	}
}

// mutateField changes fd on m in a way the verifier must notice.
func mutateField(t *testing.T, m proto.Message, fd protoreflect.FieldDescriptor) {
	t.Helper()
	r := m.ProtoReflect()
	switch {
	case fd.IsList():
		l := r.Mutable(fd).List()
		l.Append(newListElem(l, fd))
	case fd.Kind() == protoreflect.StringKind:
		r.Set(fd, protoreflect.ValueOfString(r.Get(fd).String()+"x"))
	case fd.Kind() == protoreflect.BytesKind:
		r.Set(fd, protoreflect.ValueOfBytes(append(append([]byte{}, r.Get(fd).Bytes()...), 'x')))
	case fd.Kind() == protoreflect.BoolKind:
		r.Set(fd, protoreflect.ValueOfBool(!r.Get(fd).Bool()))
	case fd.Kind() == protoreflect.Int32Kind:
		r.Set(fd, protoreflect.ValueOfInt32(int32(r.Get(fd).Int())+1))
	case fd.Kind() == protoreflect.DoubleKind:
		r.Set(fd, protoreflect.ValueOfFloat64(r.Get(fd).Float()+1))
	case fd.Kind() == protoreflect.MessageKind:
		// Oneof block members (RequestBlock.text etc.) and nested message
		// fields: replace with a fresh empty message of the same kind — a
		// change the verifier must notice (the original had content).
		mt, merr := protoregistry.GlobalTypes.FindMessageByName(fd.Message().FullName())
		if merr != nil {
			t.Fatalf("no message type for %s: %v", fd.Name(), merr)
		}
		r.Set(fd, protoreflect.ValueOfMessage(mt.New()))
	default:
		t.Fatalf("no mutation strategy for %s of kind %s", fd.Name(), fd.Kind())
	}
}

func newListElem(l protoreflect.List, fd protoreflect.FieldDescriptor) protoreflect.Value {
	if fd.Kind() == protoreflect.MessageKind {
		return l.NewElement()
	}
	return protoreflect.ValueOfString("x")
}

// A plugin changing a host-owned field is a violation regardless of grants.
func TestHostOwnedFieldChangeIsAViolation(t *testing.T) {
	accepted := baseRequest()
	out := baseRequest()
	out.ToranaMetaJson = []byte(`{"_provider":"evil"}`)

	c := compareSections(accepted, out)
	if !c.hostOwned {
		t.Fatal("a change to torana_meta_json must be reported as host-owned")
	}
	if !c.any() {
		t.Fatal("a host-owned change must count as a change")
	}
}

// The presence collision reported on #231: writing only the fields that are set
// makes these two indistinguishable unless identity and presence are framed.
func TestOptionalPresenceIsNotForgeable(t *testing.T) {
	zero32 := int32(0)
	zero64 := 0.0

	a := baseRequest()
	a.MaxTokens, a.Temperature = &zero32, nil
	b := baseRequest()
	b.MaxTokens, b.Temperature = nil, &zero64

	if fingerprintRequestSections(a).equal(fingerprintRequestSections(b)) {
		t.Fatal("presence of one optional field is forgeable as another's value")
	}
	if !compareSections(a, b).any() {
		t.Fatal("exact comparison missed a presence difference")
	}
}

// -0.0 and +0.0 are equal under ==, so a sign flip would pass unnoticed.
func TestNegativeZeroIsDetected(t *testing.T) {
	pos, neg := 0.0, math.Copysign(0, -1)

	a := baseRequest()
	a.Temperature = &pos
	b := baseRequest()
	b.Temperature = &neg

	if !compareSections(a, b).any() {
		t.Fatal("exact comparison treated -0.0 and +0.0 as identical")
	}
	if fingerprintRequestSections(a).equal(fingerprintRequestSections(b)) {
		t.Fatal("fingerprint treated -0.0 and +0.0 as identical")
	}
}

// --- F1: per-message typed records (round-1 structural ambiguity) -----------

// oldSchemeRoleDigest replicates the round-1 per-role framing — message
// fields and tool-call fields concatenated into ONE role stream — so the
// tests below can demonstrate the structural ambiguity it had: a tool call's
// four fields were byte-indistinguishable from the leading fields of the next
// same-role message, so moving the call between two messages left the
// preimage identical.
func oldSchemeRoleDigest(req *pb.ChatRequest, role string) [32]byte {
	h := sha256.New()
	for i, m := range req.Messages {
		if m.Role != role {
			continue
		}
		var idx [8]byte
		binary.LittleEndian.PutUint64(idx[:], uint64(i))
		// The round-1 framing, expressed over the block model: message
		// fields and tool-call fields concatenated into ONE role stream.
		// Ten message fields + index + role = TWELVE frames per message, so
		// a moved call (four frames) keeps the whole stream at a multiple
		// of four — the exact periodicity the collision needs.
		writeFramed(h, idx[:], []byte(m.Role), []byte(oldSchemeText(m)), []byte(oldSchemeContentParts(m)),
			[]byte(oldSchemeThinking(m)), []byte(oldSchemeThinkingSig(m)), []byte(oldSchemeContentSig(m)),
			[]byte(oldSchemeTrailingSig(m)), []byte(oldSchemeRedacted(m)), []byte(oldSchemeToolResultID(m)),
			[]byte(oldSchemeToolResultName(m)), oldSchemeCacheMarker(m))
		for _, b := range m.Blocks {
			if b.GetToolUse() != nil {
				writeFramed(h, []byte(b.GetToolUse().Id), []byte(b.GetToolUse().Name),
					b.GetToolUse().ArgumentsJson, []byte(b.GetToolUse().Signature))
			}
		}
	}
	var count [8]byte
	binary.LittleEndian.PutUint64(count[:], uint64(len(req.Messages)))
	writeFramed(h, count[:])
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// The round-1 field derivations from the block model. Each reads the FIRST
// occurrence of the corresponding block kind; the boundary-shift fixtures
// place exactly one of each.
func oldSchemeText(m *pb.Message) string { return firstText(m).GetText() }
func oldSchemeContentParts(m *pb.Message) string {
	// The round-1 content_parts slot, derived from the SECOND text block
	// (absent when there is none): the replica needs ten message fields to
	// reproduce the twelve-frames-per-message round-1 layout.
	var seen bool
	for _, b := range m.Blocks {
		if b.GetText() != nil {
			if seen {
				return b.GetText().GetText()
			}
			seen = true
		}
	}
	return ""
}
func oldSchemeThinking(m *pb.Message) string    { return firstThinking(m).GetText() }
func oldSchemeThinkingSig(m *pb.Message) string { return firstThinking(m).GetSignature() }
func oldSchemeContentSig(m *pb.Message) string  { return firstText(m).GetSignature() }
func oldSchemeTrailingSig(m *pb.Message) string { return firstTrailing(m).GetSignature() }
func oldSchemeRedacted(m *pb.Message) string    { return firstRedacted(m).GetData() }
func oldSchemeToolResultID(m *pb.Message) string {
	if tr := firstToolResult(m); tr != nil {
		return tr.ToolCallId
	}
	return ""
}
func oldSchemeToolResultName(m *pb.Message) string {
	if tr := firstToolResult(m); tr != nil {
		return tr.ToolName
	}
	return ""
}
func oldSchemeCacheMarker(m *pb.Message) []byte {
	for _, b := range m.Blocks {
		if b.GetCacheBreakpoint() != nil {
			return b.GetCacheBreakpoint().MarkerJson
		}
	}
	return nil
}
func firstText(m *pb.Message) *pb.RequestTextBlock {
	for _, b := range m.Blocks {
		if b.GetText() != nil {
			return b.GetText()
		}
	}
	return &pb.RequestTextBlock{}
}
func firstThinking(m *pb.Message) *pb.RequestThinkingBlock {
	for _, b := range m.Blocks {
		if b.GetThinking() != nil {
			return b.GetThinking()
		}
	}
	return &pb.RequestThinkingBlock{}
}
func firstTrailing(m *pb.Message) *pb.RequestTrailingSignatureBlock {
	for _, b := range m.Blocks {
		if b.GetTrailingSignature() != nil {
			return b.GetTrailingSignature()
		}
	}
	return &pb.RequestTrailingSignatureBlock{}
}
func firstRedacted(m *pb.Message) *pb.RequestRedactedThinkingBlock {
	for _, b := range m.Blocks {
		if b.GetRedactedThinking() != nil {
			return b.GetRedactedThinking()
		}
	}
	return &pb.RequestRedactedThinkingBlock{}
}
func firstToolResult(m *pb.Message) *pb.RequestToolResultBlock {
	for _, b := range m.Blocks {
		if b.GetToolResult() != nil {
			return b.GetToolResult()
		}
	}
	return nil
}

// boundaryShiftMessages builds the reviewer's reproduction: a same-role pair
// where the second message is PERIODIC with the moved call's fields, so under
// the round-1 framing the call's four fields are byte-identical to the next
// message's leading fields. accepted has the call on message 0; out moves it
// into message 1's tool-call list.
//
// The period is: idx1 (the 8-byte little-endian index of message 1) followed
// by the call's fields (id=idx1, name="user", arguments="S", signature="T"),
// cycling through the message's eleven fields. Moving the call then merely
// rotates the boundary between the concatenated frames, leaving the round-1
// preimage identical.
func boundaryShiftMessages() (accepted, out *pb.ChatRequest) {
	idx1 := string([]byte{1, 0, 0, 0, 0, 0, 0, 0})
	// The round-1 framing concatenates each message's index, role, nine
	// message fields and its tool-call frames into ONE byte stream. The
	// periodic message below is built so that stream is exactly four
	// repetitions of (idx1, "user", "S", "T") whether the call sits on
	// message 0 or message 1: the call's four frames (idx1, user, S, T)
	// are byte-identical to the next message's leading frames, and the
	// message fields cycle (S, T, idx1, user) — so moving the call between
	// the messages leaves the round-1 preimage identical. The fixture is
	// deliberately NOT a validated request (raw fixture bytes).
	call := &pb.RequestToolUseBlock{Id: idx1, Name: "user", ArgumentsJson: []byte("S"), Signature: "T"}
	// Message fields cycle (S, T, idx1, user) — text, contentParts (second
	// text block), thinking, thinkingSig, contentSig, trailingSig, redacted,
	// toolResultID, toolResultName, cacheMarker — so the twelve frames of
	// index+role+fields are exactly three periods, and the whole stream
	// (plus the moved four-frame call) stays at a multiple of four.
	periodic := &pb.Message{Role: "user", Blocks: []*pb.RequestBlock{
		{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "S", Signature: "S"}}},
		{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "T"}}},
		{Kind: &pb.RequestBlock_Thinking{Thinking: &pb.RequestThinkingBlock{Text: idx1, Signature: "user"}}},
		{Kind: &pb.RequestBlock_TrailingSignature{TrailingSignature: &pb.RequestTrailingSignatureBlock{Signature: "T"}}},
		{Kind: &pb.RequestBlock_RedactedThinking{RedactedThinking: &pb.RequestRedactedThinkingBlock{Data: idx1}}},
		{Kind: &pb.RequestBlock_ToolResult{ToolResult: &pb.RequestToolResultBlock{ToolCallId: "user", ToolName: "S", Content: []*pb.ToolResultContentBlock{{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "T"}}}}}}},
		{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: []byte("T")}}},
	}}

	accepted = &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "user", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "M0"}}},
			{Kind: &pb.RequestBlock_ToolUse{ToolUse: call}},
		}},
		periodic,
	}}
	out = &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "user", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "M0"}}}}}, // call removed
	}}
	moved := proto.Clone(periodic).(*pb.Message)
	moved.Blocks = append(moved.Blocks, &pb.RequestBlock{Kind: &pb.RequestBlock_ToolUse{ToolUse: call}})
	out.Messages = append(out.Messages, moved)
	return accepted, out
}

// The exact round-1 reviewer reproduction: two same-role messages with a
// periodic field pattern, where moving a tool call from message 0's tool-call
// list into message 1's yields an IDENTICAL round-1 preimage — so the round-1
// fingerprint returned equal digests and verifyRequestMutation returned nil
// with no grants. The per-message typed-record framing must make the shift
// visible, and verification must reject it.
func TestMessageFingerprintUnambiguousAcrossBoundaryShift(t *testing.T) {
	accepted, out := boundaryShiftMessages()

	// 1. The exact oracle sees the change.
	if !compareSections(accepted, out).any() {
		t.Fatal("exact comparison missed the boundary shift")
	}
	// 2. The round-1 framing collided — this is the reproduced bug. If this
	// assertion ever starts failing, the construction no longer reproduces
	// the reviewer's finding and the regression is silently weaker.
	if oldSchemeRoleDigest(accepted, "user") != oldSchemeRoleDigest(out, "user") {
		t.Fatal("test construction broken: the round-1 preimages must be identical " +
			"for this to reproduce the reviewer's reproduction")
	}
	// 3. The production fingerprint must NOT collide.
	if fingerprintRequestSections(accepted).equal(fingerprintRequestSections(out)) {
		t.Fatal("message fingerprint still ambiguous across a tool-call boundary shift")
	}
	// 4. verifyRequestMutation must reject with NO grants.
	err := verifyRequestMutation(accepted, out, grant())
	if err == nil {
		t.Fatal("the boundary shift must be rejected without any grant")
	}
	if !strings.Contains(err.Error(), "messages.user") {
		t.Errorf("error = %q, want the messages.user section named", err)
	}
}

// The same boundary shift across THREE messages: the call moves from message
// 0 to message 2, crossing a periodic middle. The role hasher receives
// (absolute index, message digest) per message, so the shift is visible even
// though the moved frames could otherwise be re-aligned against the periodic
// pattern — each message's digest changes where its tool-call list changed,
// and the index pins every digest to its position.
func TestMessageFingerprintBoundaryShiftAcrossThreeMessages(t *testing.T) {
	idx1 := string([]byte{1, 0, 0, 0, 0, 0, 0, 0})
	call := &pb.RequestToolUseBlock{Id: idx1, Name: "user", ArgumentsJson: []byte("S"), Signature: "T"}
	periodic := &pb.Message{Role: "user", Blocks: []*pb.RequestBlock{
		{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "S", Signature: "S"}}},
		{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "T"}}},
		{Kind: &pb.RequestBlock_Thinking{Thinking: &pb.RequestThinkingBlock{Text: idx1, Signature: "user"}}},
		{Kind: &pb.RequestBlock_TrailingSignature{TrailingSignature: &pb.RequestTrailingSignatureBlock{Signature: "T"}}},
		{Kind: &pb.RequestBlock_RedactedThinking{RedactedThinking: &pb.RequestRedactedThinkingBlock{Data: idx1}}},
		{Kind: &pb.RequestBlock_ToolResult{ToolResult: &pb.RequestToolResultBlock{ToolCallId: "user", ToolName: "S", Content: []*pb.ToolResultContentBlock{{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "T"}}}}}}},
		{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: []byte("T")}}},
	}}

	accepted := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "user", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "M0"}}},
			{Kind: &pb.RequestBlock_ToolUse{ToolUse: call}},
		}},
		periodic,
		proto.Clone(periodic).(*pb.Message),
	}}
	out := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "user", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "M0"}}}}},
		proto.Clone(periodic).(*pb.Message),
	}}
	last := proto.Clone(periodic).(*pb.Message)
	last.Blocks = append(last.Blocks, &pb.RequestBlock{Kind: &pb.RequestBlock_ToolUse{ToolUse: call}})
	out.Messages = append(out.Messages, last)

	if !compareSections(accepted, out).any() {
		t.Fatal("exact comparison missed the three-message boundary shift")
	}
	if fingerprintRequestSections(accepted).equal(fingerprintRequestSections(out)) {
		t.Fatal("fingerprint missed a tool-call boundary shift across three messages")
	}
	if err := verifyRequestMutation(accepted, out, grant()); err == nil {
		t.Fatal("the three-message boundary shift must be rejected without any grant")
	}
}
