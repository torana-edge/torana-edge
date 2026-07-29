package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"math"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/torana-edge/torana-plugin-sdk/pb"
)

// Prototypes of the write-grant verifier, so the benchmarks price the work the
// real thing has to do rather than a cheaper approximation of it.
//
// This is enforcement, and the plugin author is the threat model: the verifier
// decides whether a plugin exceeded its grants. A method that merely usually
// notices a change is not a candidate. An earlier prototype folded a per-role
// accumulator and concatenated fields unframed, which missed a cross-role
// reorder entirely and collided across field boundaries — both reproduced in
// the tests below, both disqualifying.
//
// Two safe candidates are measured:
//
//   - compareSections: exact structural comparison, no hashing. Cannot collide
//     because it never summarises.
//   - fingerprintSectionsSafe: SHA-256 over length-framed fields with the
//     message index folded in. Collision-resistant and reorder-sensitive.
//
// Exact comparison needs the accepted request kept decoded; fingerprints need
// only 32 bytes per section carried forward. That is the real trade, and the
// numbers decide it.

// changedSections names the grantable sections a plugin's output differs in.
type changedSections struct {
	messages map[string]bool // role → changed
	tools    bool
	model    bool
	params   bool
	// hostOwned marks a change to a field no grant covers, because the host
	// owns it. torana_meta_json is the only one: the host writes _provider and
	// friends into it, and under v2 verdicts are host calls rather than keys in
	// it, so a plugin has no legitimate reason to alter it at all. There is no
	// grant that permits this — it is a violation outright.
	hostOwned bool
}

func (c changedSections) any() bool {
	return len(c.messages) > 0 || c.tools || c.model || c.params || c.hostOwned
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
		case !proto.Equal(a, b):
			c.messages[a.Role] = true
			c.messages[b.Role] = true
		}
	}

	if len(accepted.Tools) != len(out.Tools) {
		c.tools = true
	} else {
		for i := range accepted.Tools {
			if !proto.Equal(accepted.Tools[i], out.Tools[i]) {
				c.tools = true
				break
			}
		}
	}

	c.model = accepted.Model != out.Model
	c.params = !sameParams(accepted, out)
	c.hostOwned = !bytes.Equal(accepted.ToranaMetaJson, out.ToranaMetaJson)
	return c
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

// safePrints is the fingerprint alternative: 32 bytes per grantable section.
type safePrints struct {
	messages map[string][32]byte
	tools    [32]byte
	model    [32]byte
	params   [32]byte
}

func (p safePrints) equal(q safePrints) bool {
	if len(p.messages) != len(q.messages) {
		return false
	}
	for role, sum := range p.messages {
		if q.messages[role] != sum {
			return false
		}
	}
	return p.tools == q.tools && p.model == q.model && p.params == q.params
}

// writeFramed length-prefixes every field, so field boundaries cannot be moved
// without changing the digest. Concatenating raw bytes lets ("ab","") and
// ("a","b") hash identically.
//
// Length framing alone is not enough for OPTIONAL fields: writing only the ones
// that are present makes {MaxTokens: 0, Temperature: nil} and
// {MaxTokens: nil, Temperature: 0} produce identical input. Use writeField for
// those — it carries the field's identity and its presence.
func writeFramed(h hash.Hash, parts ...[]byte) {
	var lenBuf [8]byte
	for _, p := range parts {
		binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(p)))
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write(p)
	}
}

// writeField frames a value together with which field it is and whether it was
// set at all, so neither identity nor presence can be forged by rearrangement.
func writeField(h hash.Hash, field byte, present bool, value []byte) {
	presence := byte(0)
	if present {
		presence = 1
	}
	_, _ = h.Write([]byte{field, presence})
	writeFramed(h, value)
}

func fingerprintSectionsSafe(req *pb.ChatRequest) safePrints {
	p := safePrints{messages: map[string][32]byte{}}

	// One hasher per role, fed each message's ABSOLUTE index in the full
	// message list. Folding only a role's own subsequence leaves a cross-role
	// reorder invisible, because neither role's subsequence changes.
	hashers := map[string]hash.Hash{}
	for i, m := range req.Messages {
		h, ok := hashers[m.Role]
		if !ok {
			h = sha256.New()
			hashers[m.Role] = h
		}
		var idx [8]byte
		binary.LittleEndian.PutUint64(idx[:], uint64(i))
		writeFramed(h, idx[:], []byte(m.Role), []byte(m.Content), m.ContentPartsJson,
			[]byte(m.Thinking), []byte(m.ThinkingSignature), []byte(m.RedactedThinking),
			[]byte(m.ToolCallId), []byte(m.ToolName), m.CacheControlJson)
		for _, tc := range m.ToolCalls {
			writeFramed(h, []byte(tc.Id), []byte(tc.Name), tc.ArgumentsJson, []byte(tc.Signature))
		}
	}
	// The message count is folded into every role, so appending or removing a
	// message of one role is visible to all of them — otherwise a deletion that
	// shifts later indices could be attributed to the wrong role alone.
	var count [8]byte
	binary.LittleEndian.PutUint64(count[:], uint64(len(req.Messages)))
	for role, h := range hashers {
		writeFramed(h, count[:])
		var sum [32]byte
		copy(sum[:], h.Sum(nil))
		p.messages[role] = sum
	}

	h := sha256.New()
	for _, t := range req.Tools {
		strict := byte(0)
		if t.Strict {
			strict = 1
		}
		writeFramed(h, []byte(t.Name), []byte(t.Description), t.ParametersJson,
			t.CacheControlJson, []byte{strict})
	}
	copy(p.tools[:], h.Sum(nil))

	h = sha256.New()
	writeFramed(h, []byte(req.Model))
	copy(p.model[:], h.Sum(nil))

	h = sha256.New()
	var scratch [8]byte
	if req.MaxTokens != nil {
		binary.LittleEndian.PutUint64(scratch[:], uint64(uint32(*req.MaxTokens)))
		writeField(h, 1, true, scratch[:])
	} else {
		writeField(h, 1, false, nil)
	}
	if req.Temperature != nil {
		// Bit pattern, not value: -0.0 and +0.0 compare equal as floats, so a
		// sign flip would be invisible.
		binary.LittleEndian.PutUint64(scratch[:], math.Float64bits(*req.Temperature))
		writeField(h, 2, true, scratch[:])
	} else {
		writeField(h, 2, false, nil)
	}
	if req.TopP != nil {
		binary.LittleEndian.PutUint64(scratch[:], math.Float64bits(*req.TopP))
		writeField(h, 3, true, scratch[:])
	} else {
		writeField(h, 3, false, nil)
	}
	stream := byte(0)
	if req.Stream {
		stream = 1
	}
	writeField(h, 4, true, []byte{stream})
	writeField(h, 5, true, req.ProviderExtensionsJson)
	writeField(h, 6, true, req.SafetySettingsJson)
	_, _ = h.Write([]byte{7})
	for _, s := range req.StopSequences {
		writeFramed(h, []byte(s))
	}
	binary.LittleEndian.PutUint64(scratch[:], uint64(len(req.StopSequences)))
	writeFramed(h, scratch[:])
	copy(p.params[:], h.Sum(nil))

	return p
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
			{Role: "system", Content: "you are helpful"},
			{Role: "user", Content: "A"},
			{Role: "assistant", ToolCalls: []*pb.ToolCall{{Id: "c1", Name: "read", ArgumentsJson: []byte(`{"p":1}`)}}},
			{Role: "tool", ToolCallId: "c1", ToolName: "read", Content: "B"},
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
			apply:     func(r *pb.ChatRequest) { r.Messages[3].Content = "B'" },
		},
		{
			// Unframed concatenation makes these two indistinguishable.
			name:      "field boundary shift",
			wantRoles: []string{"tool"},
			apply: func(r *pb.ChatRequest) {
				r.Messages[3].ToolName = "read" + r.Messages[3].Content
				r.Messages[3].Content = ""
			},
		},
		{
			name:      "message inserted",
			wantRoles: []string{"user"},
			apply: func(r *pb.ChatRequest) {
				r.Messages = append(r.Messages, &pb.Message{Role: "user", Content: "C"})
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
			apply:     func(r *pb.ChatRequest) { r.Messages[2].ToolCalls[0].ArgumentsJson = []byte(`{"p":2}`) },
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
			accepted := fingerprintSectionsSafe(baseRequest())
			out := baseRequest()
			m.apply(out)

			if accepted.equal(fingerprintSectionsSafe(out)) {
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
	if !fingerprintSectionsSafe(baseRequest()).equal(fingerprintSectionsSafe(baseRequest())) {
		t.Fatal("unmodified request produced different fingerprints")
	}
}

// --- field inventory --------------------------------------------------------

// hostOwnedField marks a field no write grant covers.
const hostOwnedField = "<host-owned>"

// Every field of every message the verifier inspects, mapped to the grant
// section that governs it.
//
// This exists because "the mutation suite covers every change" was asserted and
// wrong: the first version silently ignored provider_extensions_json,
// safety_settings_json, torana_meta_json and ToolDef.strict. A hand-written
// mutation list cannot prove coverage — it only demonstrates the cases someone
// thought of. Reflection over the descriptor can, and will fail the moment v2
// adds a field to the contract without deciding which grant governs it.
var chatRequestFieldSections = map[string]string{
	"model":                    "ir.model.write",
	"messages":                 "ir.messages.write.<role>",
	"tools":                    "ir.tools.write",
	"stream":                   "ir.params.write",
	"max_tokens":               "ir.params.write",
	"temperature":              "ir.params.write",
	"top_p":                    "ir.params.write",
	"stop_sequences":           "ir.params.write",
	"provider_extensions_json": "ir.params.write",
	"safety_settings_json":     "ir.params.write",
	"torana_meta_json":         hostOwnedField,
}

var messageFieldSections = map[string]string{
	"role":               "ir.messages.write.<role>",
	"content":            "ir.messages.write.<role>",
	"content_parts_json": "ir.messages.write.<role>",
	"thinking":           "ir.messages.write.<role>",
	"thinking_signature": "ir.messages.write.<role>",
	"redacted_thinking":  "ir.messages.write.<role>",
	"tool_calls":         "ir.messages.write.<role>",
	"tool_call_id":       "ir.messages.write.<role>",
	"tool_name":          "ir.messages.write.<role>",
	"cache_control_json": "ir.messages.write.<role>",
}

var toolCallFieldSections = map[string]string{
	"id":             "ir.messages.write.<role>",
	"name":           "ir.messages.write.<role>",
	"arguments_json": "ir.messages.write.<role>",
	"signature":      "ir.messages.write.<role>",
}

var toolDefFieldSections = map[string]string{
	"name":               "ir.tools.write",
	"description":        "ir.tools.write",
	"parameters_json":    "ir.tools.write",
	"strict":             "ir.tools.write",
	"cache_control_json": "ir.tools.write",
}

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
		{"ToolCall", func(r *pb.ChatRequest) proto.Message { return r.Messages[2].ToolCalls[0] }, toolCallFieldSections},
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
				if fingerprintSectionsSafe(accepted).equal(fingerprintSectionsSafe(out)) {
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

	if fingerprintSectionsSafe(a).equal(fingerprintSectionsSafe(b)) {
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
	if fingerprintSectionsSafe(a).equal(fingerprintSectionsSafe(b)) {
		t.Fatal("fingerprint treated -0.0 and +0.0 as identical")
	}
}
