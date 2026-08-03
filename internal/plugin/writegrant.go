package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	plugin_sdk "github.com/torana-edge/torana-plugin-sdk"
	"hash"
	"math"
	"sort"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/outboundpolicy"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// Write-grant verification for the request path.
//
// A plugin handed a ChatRequest may change only the sections its operator
// granted it: messages by role (ir.messages.write.<role>), tool definitions
// (ir.tools.write), the model (ir.model.write) and sampling params
// (ir.params.write). Everything else is host-owned and immutable under plugin
// mutation — torana_meta_json is host state (the host writes _provider and
// friends into it, and under v2 verdicts are host calls rather than keys in
// it), so changing it is a violation outright, with no grant that permits it.
//
// Bound provider signatures (Message.thinking_signature,
// Message.content_signature, Message.trailing_signature) are the ONE
// host-owned exception, classified by outboundpolicy.ClassifySignatureMutation
// rather than restated here. On the RESPONSE path a stale token (kept over
// changed covered content) is tolerated because the apply block invalidates
// the wire token before it ships; on the REQUEST path there is no apply block
// and no wire token — the plugin's output IS what goes upstream, so SignatureStale
// is a violation there. A plugin must clear the token itself when it changes
// covered content.
//
// Change detection is the section fingerprint (requestSections): SHA-256 over
// length-framed fields with the message index folded in, per role and per
// section. Each MESSAGE is hashed first as its own typed record
// (fingerprintMessage): every message field framed with tag + presence +
// length, then an explicit tool-call count, then one nested record per tool
// call. The role hasher then receives (absolute index, message digest), so a
// tool call's four fields can never be confused with a neighbouring message's
// fields — the round-1 framing was structurally ambiguous and let a call move
// between two same-role messages with an identical preimage (reproduced in
// TestMessageFingerprintUnambiguousAcrossBoundaryShift). It is
// collision-resistant and reorder-sensitive, and carries only 32 bytes per
// section forward. The exact-comparison alternative (compareSections) remains
// in writegrant_prototype_test.go as the mutation suite's oracle.
//
// Check order, first error wins (documented because both may legitimately
// fire — a signature field is also in the message fingerprint, so a signature
// mutation flags the message section AND the binding check):
//
//  1. unconditional invariants — unknown protobuf fields (any nesting level),
//     host-owned fields (torana_meta_json) and the request-domain signature
//     bindings (stale/forged/added/dropped/reused); these are checked by
//     verifyUnconditionalInvariants and can never be authorised by a grant;
//  2. changed sections without their grant (verifyGrantedSections).
//
// A fully-granted plugin skips only the section comparison via
// holdsAllRequestGrants: grants authorise SECTIONS, never host facts or
// provenance, so the all-grants fast path (verifyFastPath) still runs
// verifyUnconditionalInvariants.

// requestSections is the fingerprint of one request: 32 bytes per grantable
// section, plus a per-role message map.
type requestSections struct {
	messages     map[string][32]byte
	tools        [32]byte
	model        [32]byte
	params       [32]byte
	cacheControl [32]byte
}

func (p requestSections) equal(q requestSections) bool {
	if len(p.messages) != len(q.messages) {
		return false
	}
	for role, sum := range p.messages {
		if q.messages[role] != sum {
			return false
		}
	}
	return p.tools == q.tools && p.model == q.model && p.params == q.params && p.cacheControl == q.cacheControl
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

// fingerprintMessage digests ONE message as a typed record under a fresh
// sha256, so a message's fields cannot be confused with any other message's.
//
// The record is the SHARED SDK body fingerprint (role + ordered blocks:
// kind, presence, order, identities, exact raw bytes, signatures, nested
// tool-result content) computed over the body with the cache-breakpoint
// blocks REMOVED — cache facts are governed by ir.cache_control.write, a
// section of its own (fingerprintCacheControlSection), never by the
// message-role grants. Role sections must not see marker changes, or a
// cache-economics plugin would need every role grant. The cache section
// covers marker values AND positions, so a content/topology change that
// shifts a cache block changes BOTH sections — the union obligation.
func fingerprintMessage(m *pb.Message) [32]byte {
	body := bodyWithoutCacheBlocks(m)
	h := sha256.New()
	writeField(h, 1, m.Role != "", []byte(m.Role))
	// The SDK fingerprint is ERROR-RETURNING: a verification failure
	// (unrepresentable body) must FAIL CLOSED — the zero digest can never
	// match a genuine grant digest, so the grant check errors on the
	// caller side instead of silently hashing a partial body.
	s, err := plugin_sdk.RequestBlocksFingerprint(body)
	if err != nil {
		return [32]byte{}
	}
	writeFramed(h, []byte(s))
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// bodyWithoutCacheBlocks returns the message with its cache-breakpoint
// carriers removed — the role-section view of the ordered body. This is
// RECURSIVE: a top-level RequestBlock.cache_breakpoint is dropped, and a
// ToolResult block is deep-copied with its nested
// ToolResultContentBlock.cache_breakpoint elements dropped while every other
// nested element AND its order survive. Cache facts are governed by
// ir.cache_control.write alone (fingerprintCacheControlSection), never by
// the message-role grants; a nested marker must therefore be invisible to
// the role view exactly like a top-level one.
func bodyWithoutCacheBlocks(m *pb.Message) *pb.Message {
	out := &pb.Message{Role: m.Role}
	for _, b := range m.Blocks {
		if b.GetCacheBreakpoint() != nil {
			continue
		}
		if tr := b.GetToolResult(); tr != nil {
			cloned := proto.Clone(b).(*pb.RequestBlock)
			trOut := cloned.GetToolResult()
			trOut.Content = nestedWithoutCache(tr.Content)
			out.Blocks = append(out.Blocks, cloned)
			continue
		}
		out.Blocks = append(out.Blocks, b)
	}
	return out
}

// nestedWithoutCache returns the nested tool-result content with every
// cache-breakpoint element removed, preserving all other elements and their
// order. The input is not mutated.
func nestedWithoutCache(content []*pb.ToolResultContentBlock) []*pb.ToolResultContentBlock {
	out := make([]*pb.ToolResultContentBlock, 0, len(content))
	for _, c := range content {
		if c.GetCacheBreakpoint() != nil {
			continue
		}
		out = append(out, c)
	}
	return out
}

// fingerprintRequestSections digests the grantable sections of a request.
//
// One hasher per role, fed each message as (ABSOLUTE index in the full
// message list, per-message digest). The index pins the message to its
// position — a boundary shift or reorder moves a digest to a different
// index — and the digest is the typed record above, so no two messages can
// ever contribute the same bytes. Folding only a role's own subsequence
// leaves a cross-role reorder invisible, because neither role's subsequence
// changes.
func fingerprintRequestSections(req *pb.ChatRequest) requestSections {
	p := requestSections{messages: map[string][32]byte{}}

	hashers := map[string]hash.Hash{}
	for i, m := range req.Messages {
		h, ok := hashers[m.Role]
		if !ok {
			h = sha256.New()
			hashers[m.Role] = h
		}
		d := fingerprintMessage(m)
		var idx [8]byte
		binary.LittleEndian.PutUint64(idx[:], uint64(i))
		writeFramed(h, idx[:], d[:])
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

	p.cacheControl = fingerprintCacheControlSection(req)

	h := sha256.New()
	for _, t := range req.Tools {
		strict := byte(0)
		if t.Strict {
			strict = 1
		}
		// CacheControlJson is NOT part of the tools section: it is governed
		// by ir.cache_control.write (see fingerprintCacheControlSection).
		writeFramed(h, []byte(t.Name), []byte(t.Description), t.ParametersJson,
			[]byte{strict})
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

// fingerprintCacheControlSection digests the cache breakpoint markers of a
// request — the three carriers: top-level RequestBlock.cache_breakpoint,
// nested ToolResultContentBlock.cache_breakpoint, and
// ToolDef.cache_control_json — as ONE section governed by
// ir.cache_control.write. ONLY marker-carrying carriers contribute frames,
// each framed with its ABSOLUTE message index (tools: tool index) plus a
// distinct carrier tag and the position within the carrier:
// a marker moved between any two positions changes the section even when its
// bytes are unchanged, and a marker added, removed, inserted, or deleted
// changes it too. Marker-less structural changes (inserting or deleting a
// plain message) do NOT touch the section — a plugin that never looks at a
// marker must not need the grant. proto3 bytes carries no presence: a
// zero-length value IS the marker-less state, so empty and absent are the
// same and no artificial presence framing exists. No other field
// participates: content, role, tool identity/schema, model, params, and
// signatures stay in their own sections (or are host-owned).
func fingerprintCacheControlSection(req *pb.ChatRequest) [32]byte {
	h := sha256.New()
	var idx [8]byte
	var pos [8]byte
	for i, m := range req.Messages {
		for bi, b := range m.Blocks {
			if cb := b.GetCacheBreakpoint(); cb != nil {
				// Carrier 1: top-level message breakpoint. Absolute message
				// index + outer block index + bytes.
				binary.LittleEndian.PutUint64(idx[:], uint64(i))
				writeFramed(h, idx[:])
				binary.LittleEndian.PutUint64(pos[:], uint64(bi))
				writeFramed(h, pos[:])
				writeField(h, 1, true, cb.MarkerJson)
				continue
			}
			if tr := b.GetToolResult(); tr != nil {
				for ci, c := range tr.Content {
					cb := c.GetCacheBreakpoint()
					if cb == nil {
						continue
					}
					// Carrier 2: nested tool-result breakpoint. Absolute
					// message index + outer block index + NESTED content
					// index + bytes — a marker moved between nested
					// positions (or between carriers) changes the section.
					binary.LittleEndian.PutUint64(idx[:], uint64(i))
					writeFramed(h, idx[:])
					binary.LittleEndian.PutUint64(pos[:], uint64(bi))
					writeFramed(h, pos[:])
					binary.LittleEndian.PutUint64(pos[:], uint64(ci))
					writeFramed(h, pos[:])
					writeField(h, 2, true, cb.MarkerJson)
				}
			}
		}
	}
	for i, t := range req.Tools {
		if len(t.CacheControlJson) == 0 {
			continue
		}
		// Carrier 3: tool-definition breakpoint.
		binary.LittleEndian.PutUint64(idx[:], uint64(i))
		writeFramed(h, idx[:])
		writeField(h, 3, true, t.CacheControlJson)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// verifyUnknownInTree walks a message tree (blocks, leaves, nested
// tool-result content) refusing unknown protobuf fields at every level.
func verifyUnknownInTree(m protoreflect.Message, path string) error {
	if len(m.GetUnknown()) > 0 {
		return fmt.Errorf("plugin wrote unknown fields in %s", path)
	}
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.Kind() != protoreflect.MessageKind {
			continue
		}
		if fd.IsList() {
			list := m.Get(fd).List()
			for j := 0; j < list.Len(); j++ {
				elem := list.Get(j).Message()
				if !elem.IsValid() {
					continue
				}
				if err := verifyUnknownInTree(elem, fmt.Sprintf("%s.%s[%d]", path, fd.Name(), j)); err != nil {
					return err
				}
			}
		} else {
			sub := m.Get(fd).Message()
			if !sub.IsValid() {
				continue
			}
			if err := verifyUnknownInTree(sub, fmt.Sprintf("%s.%s", path, fd.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// verifyUnconditionalInvariants checks the host facts and provenance bindings
// that NO write grant authorises: unknown protobuf fields at any nesting
// level, torana_meta_json, and the request-domain signature bindings
// (stale/forged/added/dropped/reused — on this path there is no apply block to
// clear a wire token later, so stale is a violation and a plugin must clear
// the token itself when it changes covered content).
//
// This is the part of verification the all-grants fast path must NOT skip:
// grants authorise SECTIONS, never host facts or provider signatures, so a
// plugin holding every request grant can still smuggle unknown fields, forge
// host metadata or mint/replace/stale-bind a signature. Unknown fields first
// (bytes the host cannot validate at all), then host-owned meta, then
// signatures — first error wins.
func verifyUnconditionalInvariants(accepted, out *pb.ChatRequest) error {
	if err := verifyUnknownFields(accepted, out); err != nil {
		return err
	}
	// Host-owned: no grant covers torana_meta_json, so this is checked before
	// anything else and regardless of how many grants the plugin holds.
	if !bytes.Equal(accepted.ToranaMetaJson, out.ToranaMetaJson) {
		return fmt.Errorf("plugin changed host-owned torana_meta_json")
	}
	return verifyRequestSignatures(accepted, out)
}

// verifyUnknownFields rejects ANY unknown protobuf bytes in the OUTPUT
// request, at every nesting level: the top-level ChatRequest, every Message,
// every ToolCall and every ToolDef. Unknown fields are bytes whose semantics
// the host cannot validate (the schema does not know them) and no write
// section covers, so a plugin may not introduce them — not even with every
// grant. They survive marshalling into the next hook's input, so a
// hand-written guest could otherwise smuggle arbitrary bytes downstream
// untouched by every grant check. The error names the nesting path so the
// offending slot is obvious. (accepted is threaded for symmetry with the
// sibling invariants; the rule is unconditional on OUT.)
func verifyUnknownFields(accepted, out *pb.ChatRequest) error {
	if len(out.ProtoReflect().GetUnknown()) > 0 {
		return fmt.Errorf("plugin wrote unknown fields in request")
	}
	for i, m := range out.Messages {
		if m == nil {
			continue
		}
		if err := verifyUnknownInTree(m.ProtoReflect(), fmt.Sprintf("messages[%d]", i)); err != nil {
			return err
		}
	}
	for i, t := range out.Tools {
		if t == nil {
			continue
		}
		if len(t.ProtoReflect().GetUnknown()) > 0 {
			return fmt.Errorf("plugin wrote unknown fields in tools[%d]", i)
		}
	}
	return nil
}

// verifyGrantedSections compares the section fingerprints (messages, tools,
// model, params) and requires a grant for every changed section. Skipped by
// the all-grants fast path, because every section is grantable to that
// plugin; run for everyone else.
func verifyGrantedSections(accepted, out *pb.ChatRequest, canWrite func(section string) bool) error {
	acc := fingerprintRequestSections(accepted)
	res := fingerprintRequestSections(out)

	// Roles are compared over the sorted union of both sides: a role that
	// left a slot and the role that took it are both marked changed, which is
	// the conservative reading of a reorder or replacement. Sorting the union
	// makes the first reported violation deterministic — map iteration order
	// is not.
	var roles []string
	seen := make(map[string]bool, len(res.messages)+len(acc.messages))
	mark := func(role string) {
		if !seen[role] {
			seen[role] = true
			roles = append(roles, role)
		}
	}
	for role, sum := range res.messages {
		if acc.messages[role] != sum {
			mark(role)
		}
	}
	for role, sum := range acc.messages {
		if res.messages[role] != sum {
			mark(role)
		}
	}
	sort.Strings(roles)
	for _, role := range roles {
		grant := string(sdk.MessageWriteSection(role))
		if !canWrite(grant) {
			return fmt.Errorf("plugin changed messages.%s without %s", role, grant)
		}
	}

	if acc.tools != res.tools {
		if !canWrite("ir.tools.write") {
			return fmt.Errorf("plugin changed tools without ir.tools.write")
		}
	}
	if acc.cacheControl != res.cacheControl {
		if !canWrite("ir.cache_control.write") {
			return fmt.Errorf("plugin changed cache_control without ir.cache_control.write")
		}
	}
	if acc.model != res.model {
		if !canWrite("ir.model.write") {
			return fmt.Errorf("plugin changed model without ir.model.write")
		}
	}
	if acc.params != res.params {
		if !canWrite("ir.params.write") {
			return fmt.Errorf("plugin changed params without ir.params.write")
		}
	}
	return nil
}

// verifyRequestMutation is the full write-grant check: the unconditional
// invariants first (host-owned fields and request-domain signature bindings),
// then the granted sections. It is the unit-test entry point; discovery.go
// calls verifyUnconditionalInvariants alone on the all-grants fast path.
//
// Section changes are detected by fingerprinting both requests; torana_meta_json
// and the signature tokens are read directly because they are not part of any
// grantable section. Errors name the section and the missing grant so an
// operator can approve exactly what the plugin needs.
func verifyRequestMutation(accepted, out *pb.ChatRequest, canWrite func(section string) bool) error {
	if err := verifyUnconditionalInvariants(accepted, out); err != nil {
		return err
	}
	return verifyGrantedSections(accepted, out, canWrite)
}

// verifyFastPath is the exact branch discovery.go takes for a plugin holding
// every request grant: the unconditional invariants only. Grants authorise
// SECTIONS, never host facts or provenance, so the section comparison is the
// only part a fully-granted plugin can skip. Factored as its own function so
// the fast-path benchmark measures the production branch verbatim.
func verifyFastPath(accepted, out *pb.ChatRequest) error {
	return verifyUnconditionalInvariants(accepted, out)
}

// requestSignatureFields is the request-domain subset of the SDK's opaque
// signature inventory, resolved once against the BLOCK proto.
//
// AllSignatureBindings deep-copies on every call and the request bindings are
// immutable after startup (the host runs outboundpolicy.Validate as its
// startup proof), so verification pays for that copy once per process instead
// of once per plugin per request. Resolving the descriptors here also makes a
// binding that names a message/field this host does not know fail the host at
// startup rather than silently comparing nothing per request.
var requestSignatureFields = func() []requestSignatureField {
	bindings := outboundpolicy.AllSignatureBindings()
	var out []requestSignatureField
	for _, b := range bindings {
		if b.Domain != outboundpolicy.SignatureDomainRequest {
			continue
		}
		mt, err := protoregistry.GlobalTypes.FindMessageByName(b.Message)
		if err != nil {
			panic(fmt.Sprintf("writegrant: request signature binding %q names unknown message %s",
				b.SignatureField, b.Message))
		}
		fields := mt.Descriptor().Fields()
		sig := fields.ByName(protoreflect.Name(b.SignatureField))
		if sig == nil || sig.Kind() != protoreflect.StringKind {
			panic(fmt.Sprintf(
				"writegrant: request signature binding %q covers unknown or non-string %s field",
				b.SignatureField, b.Message))
		}
		scope := outboundpolicy.SignatureScopeSameMessage
		var sameRefs []resolvedRef
		var trailRefs []trailSignatureRef
		for _, ref := range b.Content {
			if ref.Scope != outboundpolicy.SignatureScopeSameMessage {
				scope = ref.Scope
			}
			if ref.Scope == outboundpolicy.SignatureScopeTrailingStandalone {
				covMt, cerr := protoregistry.GlobalTypes.FindMessageByName(ref.Message)
				if cerr != nil {
					panic(fmt.Sprintf("writegrant: trailing binding %q names unknown covered message %s",
						b.SignatureField, ref.Message))
				}
				fd := covMt.Descriptor().Fields().ByName(protoreflect.Name(ref.Field))
				if fd == nil || (fd.Kind() != protoreflect.StringKind && fd.Kind() != protoreflect.BytesKind) {
					panic(fmt.Sprintf(
						"writegrant: trailing binding %q covers unknown or non-string %s field",
						b.SignatureField, ref.Message))
				}
				trailRefs = append(trailRefs, trailSignatureRef{msg: ref.Message, fd: fd})
				continue
			}
			fd := fields.ByName(protoreflect.Name(ref.Field))
			kind, ok := classifyCoveredRef(fd, ref)
			if !ok {
				panic(fmt.Sprintf(
					"writegrant: request signature binding %q covers unsupported %s field %s",
					b.SignatureField, b.Message, ref.Field))
			}
			sameRefs = append(sameRefs, resolvedRef{fd: fd, kind: kind})
		}
		out = append(out, requestSignatureField{
			binding: b, scope: scope, name: requestTokenName(b.Message), sig: sig,
			sameRefs: sameRefs, trailRefs: trailRefs,
		})
	}
	return out
}()

// requestSignatureField is one resolved request-domain binding: the token
// field on the binding's BLOCK message, plus the covered content fields —
// the token block's own fields (SameMessage) or the preceding
// text/thinking block fields (TrailingStandalone).
type requestSignatureField struct {
	binding   outboundpolicy.SignatureBinding
	scope     outboundpolicy.SignatureScope
	name      string // semantic token name for errors: content_signature, ...
	sig       protoreflect.FieldDescriptor
	sameRefs  []resolvedRef
	trailRefs []trailSignatureRef
}

// resolvedRef is one covered SameMessage field with its digest framing
// class (REV 4 §4 covered-field-kind model: plain string/bytes framed
// value; proto3 OPTIONAL bool/string framed presence+value; the ONE pinned
// repeated-message content field digested by the SDK primitive).
type resolvedRef struct {
	fd   protoreflect.FieldDescriptor
	kind coveredRefKind
}

// coveredRefKind classifies the digest framing for a covered field.
type coveredRefKind int

const (
	refPlainString coveredRefKind = iota + 1
	refPlainBytes
	refOptionalBool
	refOptionalString
	refPinnedContent
)

// pinnedRepeatedContentTarget is the ONE repeated-message field the
// covered-kind model allows (mirrors the SDK's pinnedRepeatedContentTarget
// in outboundpolicy): its digest is the SDK's total
// ToolResultContentFingerprint, never a host re-implementation.
const pinnedRepeatedContentTarget = "torana.v2.RequestToolResultBlock.content"

// classifyCoveredRef validates a covered SameMessage field against the
// typed covered-field-kind model and returns its framing class. Repeated
// or message-typed fields fail unless they are the pinned content target;
// plain non-optional scalars other than string/bytes fail.
func classifyCoveredRef(fd protoreflect.FieldDescriptor, ref outboundpolicy.SignatureContentRef) (coveredRefKind, bool) {
	if fd == nil {
		return 0, false
	}
	switch {
	case fd.IsList() && fd.Kind() == protoreflect.MessageKind &&
		fd.FullName() == pinnedRepeatedContentTarget:
		return refPinnedContent, true
	case fd.IsList():
		return 0, false
	case fd.Kind() == protoreflect.BoolKind && fd.HasOptionalKeyword():
		return refOptionalBool, true
	case fd.Kind() == protoreflect.StringKind && fd.HasOptionalKeyword():
		return refOptionalString, true
	case fd.Kind() == protoreflect.StringKind:
		return refPlainString, true
	case fd.Kind() == protoreflect.BytesKind:
		return refPlainBytes, true
	}
	return 0, false
}

// trailSignatureRef is one covered field on a covered block kind for the
// TrailingStandalone scope (text/thinking blocks preceding the token).
type trailSignatureRef struct {
	msg protoreflect.FullName
	fd  protoreflect.FieldDescriptor
}

// requestTokenName maps a token block message to the semantic token name the
// flat model used — the vocabulary operators and tests already know.
func requestTokenName(msg protoreflect.FullName) string {
	switch msg {
	case "torana.v2.RequestTextBlock":
		return "content_signature"
	case "torana.v2.RequestThinkingBlock":
		return "thinking_signature"
	case "torana.v2.RequestToolUseBlock":
		return "tool_use_signature"
	case "torana.v2.RequestTrailingSignatureBlock":
		return "trailing_signature"
	}
	return string(msg)
}

// signedOccurrence is one accepted occurrence carrying a non-empty token
// for a binding: the digest of its covered content, and whether phase 1 has
// consumed it. The digest lets both phases compare covered content by hash
// instead of re-reading fields per output message. With the ordered body an
// occurrence is one BLOCK (SameMessage scopes: a block of the binding's
// kind carrying a signature) or one message (TrailingStandalone: the
// singular trailing block).
type signedOccurrence struct {
	digest [32]byte
	used   bool
}

// tokenOccurrence is one signature occurrence found in a message: its token
// (empty = unsigned) and the digest of its covered content.
type tokenOccurrence struct {
	token  string
	digest [32]byte
}

// requestBlockMessage returns the block's oneof message of the given type,
// or nil. The RequestBlock descriptor's oneof members are the block kinds.
var requestBlockFieldKinds = func() map[protoreflect.FullName]protoreflect.FieldDescriptor {
	fields := (&pb.RequestBlock{}).ProtoReflect().Descriptor().Fields()
	m := make(map[protoreflect.FullName]protoreflect.FieldDescriptor)
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.Message() != nil {
			m[fd.Message().FullName()] = fd
		}
	}
	return m
}()

func requestBlockMessage(b *pb.RequestBlock, want protoreflect.FullName) protoreflect.Message {
	if b == nil {
		return nil
	}
	fd, ok := requestBlockFieldKinds[want]
	if !ok {
		return nil
	}
	m := b.ProtoReflect()
	if !m.Has(fd) {
		return nil
	}
	return m.Get(fd).Message()
}

// occurrences extracts the binding's signature occurrences from a message.
// SameMessage scopes: every block of the binding's kind — each block's
// signature is its own token over its own covered fields. TrailingStandalone:
// at most one token per message (the trailing block), covering the text of
// every preceding text/thinking block in wire order. An unrepresentable
// covered value is an error (fail closed), never a zero digest that could
// silently match on both sides.
func (r requestSignatureField) occurrences(m *pb.Message) ([]tokenOccurrence, error) {
	if m == nil {
		return nil, nil
	}
	if r.scope == outboundpolicy.SignatureScopeTrailingStandalone {
		var trail protoreflect.Message
		for _, b := range m.Blocks {
			if pm := requestBlockMessage(b, r.binding.Message); pm != nil {
				trail = pm
			}
		}
		if trail == nil {
			return nil, nil
		}
		token := trail.Get(r.sig).String()
		h := sha256.New()
		for _, b := range m.Blocks {
			for _, ref := range r.trailRefs {
				if pm := requestBlockMessage(b, ref.msg); pm != nil {
					writeFramed(h, []byte(pm.Get(ref.fd).String()))
				}
			}
		}
		var sum [32]byte
		copy(sum[:], h.Sum(nil))
		return []tokenOccurrence{{token: token, digest: sum}}, nil
	}
	var out []tokenOccurrence
	for _, b := range m.Blocks {
		pm := requestBlockMessage(b, r.binding.Message)
		if pm == nil {
			continue
		}
		d, err := digestBlockFields(pm, r.sameRefs)
		if err != nil {
			return nil, fmt.Errorf("writegrant: %s occurrence: %w", r.name, err)
		}
		out = append(out, tokenOccurrence{
			token:  pm.Get(r.sig).String(),
			digest: d,
		})
	}
	return out, nil
}

// digestBlockFields hashes a block's covered-content fields, length-framed
// so field boundaries cannot be confused. Absent fields hash as empty.
// digestBlockFields hashes a block's covered-content fields, length-framed
// per the REV 4 §4 covered-field-kind model: plain string/bytes frame the
// value; proto3 OPTIONAL bool/string frame PRESENCE + value (an explicit
// false or an explicit scheduling value is a different digest than
// absence); the pinned repeated content field is digested by the SDK's
// total ToolResultContentFingerprint — the host never re-implements nested
// block semantics. An unrepresentable covered value (invalid nested
// payload) is an ERROR: the digest must fail the verification, never hash
// a partial body.
func digestBlockFields(pm protoreflect.Message, refs []resolvedRef) ([32]byte, error) {
	h := sha256.New()
	for _, ref := range refs {
		switch ref.kind {
		case refPlainString, refPlainBytes:
			writeFramed(h, []byte(pm.Get(ref.fd).String()))
		case refOptionalBool, refOptionalString:
			// proto3 optional presence is part of the covered content:
			// presence + value, exactly like the SDK fingerprint's
			// wc/wcval and sched/schedval frames.
			writeField(h, byte(ref.kind), pm.Has(ref.fd), []byte(pm.Get(ref.fd).String()))
		case refPinnedContent:
			blocks, err := contentBlocks(pm.Get(ref.fd).List())
			if err != nil {
				return [32]byte{}, err
			}
			sum, err := plugin_sdk.ToolResultContentFingerprint(blocks)
			if err != nil {
				return [32]byte{}, fmt.Errorf("writegrant: covered nested content: %w", err)
			}
			writeFramed(h, sum[:])
		default:
			return [32]byte{}, fmt.Errorf("writegrant: unknown covered ref kind %d", ref.kind)
		}
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// contentBlocks converts a protoreflect list of ToolResultContentBlock
// messages to the SDK's slice. Any element that is not the pinned message
// type is a host/proto drift error.
func contentBlocks(l protoreflect.List) ([]*pb.ToolResultContentBlock, error) {
	out := make([]*pb.ToolResultContentBlock, 0, l.Len())
	for i := 0; i < l.Len(); i++ {
		el, ok := l.Get(i).Message().Interface().(*pb.ToolResultContentBlock)
		if !ok {
			return nil, fmt.Errorf("writegrant: covered content element %d is not a ToolResultContentBlock", i)
		}
		out = append(out, el)
	}
	return out, nil
}

// verifyRequestSignatures applies the bound-signature rule to every message,
// aligning the output's signed messages ONE-TO-ONE against the accepted
// signed occurrences (a multiset alignment), WITHOUT role in the match key —
// the SDK's request contracts bind the declared content refs, not role, so a
// granted role change carrying the same token over unchanged covered content
// is intact, and role stays governed by ir.messages.write.<role> alone.
//
// Covered content is decided over the binding's whole content-ref set
// (thinking_signature covers thinking + redacted_thinking, trailing_signature
// covers thinking + content, content_signature covers content). Accepted
// occurrences are indexed by token value (phase 1) and by covered-content
// digest (phase 2), so lookups are not quadratic.
//
// Phase 1 — output messages with a non-empty token, each consuming one
// unconsumed accepted occurrence with the SAME token value:
//
//   - an exact covered-content match consumes that occurrence (intact);
//   - no match but several unconsumed candidates covering DIFFERENT content
//     is ambiguous — reject rather than guess which one was meant;
//   - no match and the remaining candidates agree (one candidate, or several
//     covering the same content) is stale — the token was kept over changed
//     content;
//   - no unconsumed occurrence with that token at all: a different non-empty
//     token over content a remaining occurrence still covers is forged
//     (replaced); a token value the accepted request carried but whose
//     occurrences were all consumed is reused — one accepted token
//     authorising a second signed message — and rejected; otherwise the
//     token was minted (added).
//
// Phase 2 — AFTER phase 1, output messages with an EMPTY token: their covered
// content compared against the REMAINING unconsumed accepted occurrences
// (any token value). An exact match means the token was dropped from content
// the provider actually signed — rejected. No match means the token was
// cleared over changed content (the prescribed response) or never existed —
// allowed.
//
// Accepted occurrences with no output counterpart at all (the signed message
// was deleted) are allowed: deletion is grantable.
func verifyRequestSignatures(accepted, out *pb.ChatRequest) error {
	for _, check := range requestSignatureFields {
		byToken := map[string][]*signedOccurrence{}
		byContent := map[[32]byte][]*signedOccurrence{}
		// Counts of accepted UNSIGNED occurrences per covered-content digest:
		// an unsigned output occurrence with an unchanged unsigned counterpart
		// is spoken for and must not be diagnosed as a dropped token (the
		// signed twin it was deleted alongside is the grantable deletion).
		unsignedByContent := map[[32]byte]int{}
		everSeen := map[string]bool{}
		for _, am := range accepted.Messages {
			occs, err := check.occurrences(am)
			if err != nil {
				return err
			}
			for _, occ := range occs {
				if occ.token == "" {
					unsignedByContent[occ.digest]++
					continue
				}
				so := &signedOccurrence{digest: occ.digest}
				byToken[occ.token] = append(byToken[occ.token], so)
				byContent[occ.digest] = append(byContent[occ.digest], so)
				everSeen[occ.token] = true
			}
		}

		// Phase 1: consume output tokens against the accepted occurrences.
		for _, om := range out.Messages {
			toccs, err := check.occurrences(om)
			if err != nil {
				return err
			}
			for _, tocc := range toccs {
				if tocc.token == "" {
					continue
				}
				outToken := tocc.token
				outDigest := tocc.digest
				matched := -1
				for i, occ := range byToken[outToken] {
					if !occ.used && occ.digest == outDigest {
						matched = i
						break
					}
				}
				if matched >= 0 {
					byToken[outToken][matched].used = true
					continue // intact
				}

				var unused []*signedOccurrence
				for _, occ := range byToken[outToken] {
					if !occ.used {
						unused = append(unused, occ)
					}
				}
				switch {
				case len(unused) == 0:
					// No unconsumed occurrence carries this token value. A
					// DIFFERENT non-empty token over content a remaining
					// occurrence still covers is a replacement — forged. A token
					// value the accepted request carried but whose occurrences
					// were all consumed is a reuse — rejected. Otherwise the
					// token was minted over content no accepted token covered —
					// added.
					for _, occ := range byContent[outDigest] {
						if !occ.used {
							return fmt.Errorf("plugin %s signature %s", check.name,
								outboundpolicy.SignatureForged)
						}
					}
					if everSeen[outToken] {
						return fmt.Errorf("plugin %s signature reused", check.name)
					}
					return fmt.Errorf("plugin %s signature %s", check.name,
						outboundpolicy.SignatureAdded)
				case conflictingContent(unused):
					return fmt.Errorf("plugin %s signature duplicate token with conflicting content",
						check.name)
				default:
					return fmt.Errorf("plugin %s signature %s", check.name,
						outboundpolicy.SignatureStale)
				}
			}
		}

		// Phase 2: unsigned outputs are consumed against accepted UNSIGNED
		// occurrences first — an output copy of content that already had an
		// unsigned twin is spoken for, and deleting the signed occurrence
		// alongside it is a grantable deletion. Only SURPLUS unsigned outputs
		// may match remaining signed occurrences: that is provenance dropped
		// from content the provider signed. Occurrences consumed in phase 1 do
		// not count, so a signed copy plus a cleared copy of the same content
		// passes: the signed occurrence is already spoken for.
		for _, om := range out.Messages {
			toccs, err := check.occurrences(om)
			if err != nil {
				return err
			}
			for _, tocc := range toccs {
				if tocc.token != "" {
					continue
				}
				if unsignedByContent[tocc.digest] > 0 {
					unsignedByContent[tocc.digest]--
					continue // unchanged unsigned counterpart exists
				}
				for _, occ := range byContent[tocc.digest] {
					if !occ.used {
						return fmt.Errorf("plugin %s signature %s", check.name,
							outboundpolicy.SignatureDropped)
					}
				}
			}
		}
	}
	return nil
}

// conflictingContent reports whether the unconsumed candidates for one token
// cover DIFFERENT content, in which case the output cannot be paired reliably
// and the mutation is ambiguous. Candidates sharing a digest are the same
// signed fact and fine.
func conflictingContent(occurrences []*signedOccurrence) bool {
	if len(occurrences) < 2 {
		return false
	}
	first := occurrences[0].digest
	for _, occ := range occurrences[1:] {
		if occ.digest != first {
			return true
		}
	}
	return false
}

// allRequestGrants is the complete set of request write grants. A plugin
// holding every one of them can change any section verification guards, so
// verification is pure cost for it.
var allRequestGrants = []string{
	"ir.cache_control.write",
	"ir.messages.write.user",
	"ir.messages.write.assistant",
	"ir.messages.write.system",
	"ir.messages.write.tool",
	"ir.messages.write.developer",
	"ir.messages.write.other",
	"ir.tools.write",
	"ir.model.write",
	"ir.params.write",
}

// grants is the narrow permission surface verification needs.
type grants interface {
	HasGrant(perm string) bool
}

// holdsAllRequestGrants reports whether the plugin holds every request write
// grant, in which case section verification is unnecessary: every section it
// could change is grantable to it. The unconditional invariants (host-owned
// fields, signature provenance) are still checked — grants never authorise
// those. ir.stream.write is deliberately absent — it governs the stream
// topology path, not the request sections verified here.
func holdsAllRequestGrants(p grants) bool {
	for _, grant := range allRequestGrants {
		if !p.HasGrant(grant) {
			return false
		}
	}
	return true
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
	"role":   "ir.messages.write.<role>",
	"blocks": "ir.messages.write.<role>",
}

// The ordered-body block messages. Content-bearing kinds are governed by the
// message role section; the cache-breakpoint carriers are governed by
// ir.cache_control.write alone (marker VALUE and POSITION — the surrounding
// content stays the role's business).
var requestBlockFieldSections = map[string]string{
	"text":               "ir.messages.write.<role>",
	"thinking":           "ir.messages.write.<role>",
	"redacted_thinking":  "ir.messages.write.<role>",
	"tool_use":           "ir.messages.write.<role>",
	"tool_result":        "ir.messages.write.<role>",
	"cache_breakpoint":   "ir.cache_control.write",
	"unknown":            "ir.messages.write.<role>",
	"trailing_signature": "ir.messages.write.<role>",
}

var requestTextBlockFieldSections = map[string]string{
	"text":               "ir.messages.write.<role>",
	"signature":          "ir.messages.write.<role>",
	"part_metadata_json": "ir.messages.write.<role>",
}

var requestThinkingBlockFieldSections = map[string]string{
	"text":               "ir.messages.write.<role>",
	"signature":          "ir.messages.write.<role>",
	"part_metadata_json": "ir.messages.write.<role>",
}

var requestRedactedThinkingBlockFieldSections = map[string]string{
	"data": "ir.messages.write.<role>",
}

var requestToolUseBlockFieldSections = map[string]string{
	"id":                 "ir.messages.write.<role>",
	"name":               "ir.messages.write.<role>",
	"arguments_json":     "ir.messages.write.<role>",
	"signature":          "ir.messages.write.<role>",
	"part_metadata_json": "ir.messages.write.<role>",
}

var requestToolResultBlockFieldSections = map[string]string{
	"tool_call_id":       "ir.messages.write.<role>",
	"tool_name":          "ir.messages.write.<role>",
	"content":            "ir.messages.write.<role>",
	"part_metadata_json": "ir.messages.write.<role>",
	"will_continue":      "ir.messages.write.<role>",
	"scheduling":         "ir.messages.write.<role>",
	"signature":          "ir.messages.write.<role>",
}

var toolResultContentBlockFieldSections = map[string]string{
	"text":             "ir.messages.write.<role>",
	"unknown":          "ir.messages.write.<role>",
	"cache_breakpoint": "ir.cache_control.write",
}

var toolResultTextBlockFieldSections = map[string]string{
	"text": "ir.messages.write.<role>",
}

var toolResultUnknownBlockFieldSections = map[string]string{
	"kind":         "ir.messages.write.<role>",
	"payload_json": "ir.messages.write.<role>",
}

var requestUnknownBlockFieldSections = map[string]string{
	"kind":               "ir.messages.write.<role>",
	"payload_json":       "ir.messages.write.<role>",
	"part_metadata_json": "ir.messages.write.<role>",
	"signature":          "ir.messages.write.<role>",
}

var requestTrailingSignatureBlockFieldSections = map[string]string{
	"signature":          "ir.messages.write.<role>",
	"part_metadata_json": "ir.messages.write.<role>",
}

var requestCacheBreakpointFieldSections = map[string]string{
	"marker_json": "ir.cache_control.write",
}

var toolResultCacheBreakpointFieldSections = map[string]string{
	"marker_json": "ir.cache_control.write",
}

// toolCallFieldSections governs the response-side ToolCall shape, which v2
// keeps for streaming/response domains. It never appears in a request; the
// request-domain tool-call carrier is RequestToolUseBlock.
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
	"cache_control_json": "ir.cache_control.write",
}
