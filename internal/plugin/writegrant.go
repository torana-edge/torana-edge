package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"math"
	"sort"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/outboundpolicy"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/reflect/protoreflect"
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
	messages map[string][32]byte
	tools    [32]byte
	model    [32]byte
	params   [32]byte
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

// fingerprintMessage digests ONE message as a typed record under a fresh
// sha256, so a message's fields cannot be confused with any other message's,
// nor with a tool call's, and a tool call cannot be confused with a message.
//
// The record is: one writeField frame per message field (tag byte + presence
// + length-framed value — presence is folded in so "field absent" and "field
// empty" are never the same input), then an explicit tool-call COUNT frame,
// then one nested record per tool call: a "call" marker frame plus the call's
// four fields, each writeField-framed. The count frame draws the boundary
// between the message's own fields and its call records, and the marker plus
// fixed four-field record keeps every call's fields from being confused with
// the message's or another call's. Frame-for-frame the record is a prefix
// code, so two different messages cannot contribute the same digest input.
//
// This is what makes the round-1 boundary shift impossible: there, message
// fields and tool-call fields were concatenated into one role stream, so a
// call's four fields were byte-indistinguishable from the leading fields of
// the next same-role message and the call could move between them with an
// identical preimage.
func fingerprintMessage(m *pb.Message) [32]byte {
	h := sha256.New()
	writeField(h, 1, m.Role != "", []byte(m.Role))
	writeField(h, 2, m.Content != "", []byte(m.Content))
	writeField(h, 3, len(m.ContentPartsJson) > 0, m.ContentPartsJson)
	writeField(h, 4, m.Thinking != "", []byte(m.Thinking))
	writeField(h, 5, m.ThinkingSignature != "", []byte(m.ThinkingSignature))
	writeField(h, 6, m.ContentSignature != "", []byte(m.ContentSignature))
	writeField(h, 7, m.TrailingSignature != "", []byte(m.TrailingSignature))
	writeField(h, 8, m.RedactedThinking != "", []byte(m.RedactedThinking))
	writeField(h, 9, m.ToolCallId != "", []byte(m.ToolCallId))
	writeField(h, 10, m.ToolName != "", []byte(m.ToolName))
	writeField(h, 11, len(m.CacheControlJson) > 0, m.CacheControlJson)

	var count [8]byte
	binary.LittleEndian.PutUint64(count[:], uint64(len(m.ToolCalls)))
	writeFramed(h, count[:])
	for _, tc := range m.ToolCalls {
		// A call record is a marker frame plus exactly four tagged fields.
		// The marker keeps a record from ever parsing as message fields, and
		// the fixed shape keeps one call from running into the next.
		writeFramed(h, []byte{callMarker})
		writeField(h, 1, tc.Id != "", []byte(tc.Id))
		writeField(h, 2, tc.Name != "", []byte(tc.Name))
		writeField(h, 3, len(tc.ArgumentsJson) > 0, tc.ArgumentsJson)
		writeField(h, 4, tc.Signature != "", []byte(tc.Signature))
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// callMarker distinguishes a tool-call record inside a message's digest from
// the message's own field frames.
const callMarker = 0x63 // 'c'

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
		if len(m.ProtoReflect().GetUnknown()) > 0 {
			return fmt.Errorf("plugin wrote unknown fields in messages[%d]", i)
		}
		for j, tc := range m.ToolCalls {
			if tc == nil {
				continue
			}
			if len(tc.ProtoReflect().GetUnknown()) > 0 {
				return fmt.Errorf("plugin wrote unknown fields in messages[%d].tool_calls[%d]", i, j)
			}
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
// signature inventory, resolved once with its Message field descriptors.
//
// AllSignatureBindings deep-copies on every call and the request bindings are
// immutable after startup (the host runs outboundpolicy.Validate as its
// startup proof), so verification pays for that copy once per process instead
// of once per plugin per request. Resolving the field descriptors here also
// makes a binding that names a Message field this host does not know fail the
// host at startup rather than silently comparing nothing per request.
var requestSignatureFields = func() []requestSignatureField {
	bindings := outboundpolicy.AllSignatureBindings()
	fields := (&pb.Message{}).ProtoReflect().Descriptor().Fields()
	var out []requestSignatureField
	for _, b := range bindings {
		if b.Domain != outboundpolicy.SignatureDomainRequest {
			continue
		}
		sig := fields.ByName(protoreflect.Name(b.SignatureField))
		refs := make([]protoreflect.FieldDescriptor, 0, len(b.Content))
		for _, ref := range b.Content {
			refs = append(refs, fields.ByName(protoreflect.Name(ref.Field)))
		}
		for _, fd := range append([]protoreflect.FieldDescriptor{sig}, refs...) {
			if fd == nil || fd.Kind() != protoreflect.StringKind {
				panic(fmt.Sprintf(
					"writegrant: request signature binding %q covers unknown or non-string Message field",
					b.SignatureField))
			}
		}
		out = append(out, requestSignatureField{binding: b, sig: sig, refs: refs})
	}
	return out
}()

// requestSignatureField is one resolved request-domain binding: the token
// field plus the same-message content fields it covers.
type requestSignatureField struct {
	binding outboundpolicy.SignatureBinding
	sig     protoreflect.FieldDescriptor
	refs    []protoreflect.FieldDescriptor
}

// signedOccurrence is one accepted message carrying a non-empty token for a
// binding: the digest of its covered content, and whether phase 1 has
// consumed it. The digest lets both phases compare covered content by hash
// instead of re-reading fields per output message.
type signedOccurrence struct {
	digest [32]byte
	used   bool
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
		// Counts of accepted UNSIGNED messages per covered-content digest:
		// an unsigned output message with an unchanged unsigned counterpart
		// is spoken for and must not be diagnosed as a dropped token (the
		// signed twin it was deleted alongside is the grantable deletion).
		unsignedByContent := map[[32]byte]int{}
		everSeen := map[string]bool{}
		for _, am := range accepted.Messages {
			if am == nil {
				continue
			}
			token := messageStringField(am, check.sig)
			if token == "" {
				unsignedByContent[coveredContentDigest(am, check.refs)]++
				continue
			}
			occ := &signedOccurrence{digest: coveredContentDigest(am, check.refs)}
			byToken[token] = append(byToken[token], occ)
			byContent[occ.digest] = append(byContent[occ.digest], occ)
			everSeen[token] = true
		}

		// Phase 1: consume output tokens against the accepted occurrences.
		for _, om := range out.Messages {
			if om == nil {
				continue
			}
			outToken := messageStringField(om, check.sig)
			if outToken == "" {
				continue
			}
			outDigest := coveredContentDigest(om, check.refs)
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
						return fmt.Errorf("plugin %s signature %s", check.binding.SignatureField,
							outboundpolicy.SignatureForged)
					}
				}
				if everSeen[outToken] {
					return fmt.Errorf("plugin %s signature reused", check.binding.SignatureField)
				}
				return fmt.Errorf("plugin %s signature %s", check.binding.SignatureField,
					outboundpolicy.SignatureAdded)
			case conflictingContent(unused):
				return fmt.Errorf("plugin %s signature duplicate token with conflicting content",
					check.binding.SignatureField)
			default:
				return fmt.Errorf("plugin %s signature %s", check.binding.SignatureField,
					outboundpolicy.SignatureStale)
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
			if om == nil || messageStringField(om, check.sig) != "" {
				continue
			}
			digest := coveredContentDigest(om, check.refs)
			if unsignedByContent[digest] > 0 {
				unsignedByContent[digest]--
				continue // unchanged unsigned counterpart exists
			}
			for _, occ := range byContent[digest] {
				if !occ.used {
					return fmt.Errorf("plugin %s signature %s", check.binding.SignatureField,
						outboundpolicy.SignatureDropped)
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

// coveredContentDigest hashes a message's covered-content fields for one
// binding, length-framed so field boundaries cannot be confused. Absent
// fields hash as empty.
func coveredContentDigest(m *pb.Message, refs []protoreflect.FieldDescriptor) [32]byte {
	h := sha256.New()
	for _, ref := range refs {
		writeFramed(h, []byte(messageStringField(m, ref)))
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// messageStringField reads a string field of a Message, or "" when the message
// is absent. fd is the pre-resolved descriptor; callers resolve it once per
// binding, never per message.
func messageStringField(m *pb.Message, fd protoreflect.FieldDescriptor) string {
	if m == nil {
		return ""
	}
	return m.ProtoReflect().Get(fd).String()
}

// allRequestGrants is the complete set of request write grants. A plugin
// holding every one of them can change any section verification guards, so
// verification is pure cost for it.
var allRequestGrants = []string{
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
	"role":               "ir.messages.write.<role>",
	"content":            "ir.messages.write.<role>",
	"content_parts_json": "ir.messages.write.<role>",
	"thinking":           "ir.messages.write.<role>",
	"thinking_signature": "ir.messages.write.<role>",
	"content_signature":  "ir.messages.write.<role>",
	"trailing_signature": "ir.messages.write.<role>",
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
