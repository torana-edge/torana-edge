package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"math"

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
// section. It is collision-resistant and reorder-sensitive, and carries only
// 32 bytes per section forward. The exact-comparison alternative (compareSections)
// remains in writegrant_prototype_test.go as the mutation suite's oracle.
//
// Check order, first error wins (documented because both may legitimately
// fire — a signature field is also in the message fingerprint, so a signature
// mutation flags the message section AND the binding check):
//
//  1. host-owned fields (torana_meta_json) — always a violation;
//  2. changed sections without their grant;
//  3. request-domain signature bindings (stale/forged/added/dropped).
//
// A fully-granted plugin skips verification entirely via holdsAllRequestGrants:
// every request section is grantable to it, so fingerprinting and comparing
// would be pure cost.

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

// fingerprintRequestSections digests the grantable sections of a request.
//
// One hasher per role, fed each message's ABSOLUTE index in the full message
// list. Folding only a role's own subsequence leaves a cross-role reorder
// invisible, because neither role's subsequence changes.
func fingerprintRequestSections(req *pb.ChatRequest) requestSections {
	p := requestSections{messages: map[string][32]byte{}}

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
			[]byte(m.Thinking), []byte(m.ThinkingSignature), []byte(m.ContentSignature),
			[]byte(m.TrailingSignature), []byte(m.RedactedThinking),
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

// verifyRequestMutation reports whether a plugin's replace_request output is a
// valid mutation of the accepted request: every changed section must be
// covered by a grant the plugin holds (canWrite), host-owned fields must be
// untouched, and request-domain signature bindings must obey
// ClassifySignatureMutation — which on this path rejects SignatureStale,
// because there is no apply block to clear the wire token later.
//
// Section changes are detected by fingerprinting both requests; torana_meta_json
// and the signature tokens are read directly because they are not part of any
// grantable section. Errors name the section and the missing grant so an
// operator can approve exactly what the plugin needs.
func verifyRequestMutation(accepted, out *pb.ChatRequest, canWrite func(section string) bool) error {
	// Host-owned: no grant covers torana_meta_json, so this is checked before
	// any section grant and regardless of how many grants the plugin holds.
	if !bytes.Equal(accepted.ToranaMetaJson, out.ToranaMetaJson) {
		return fmt.Errorf("plugin changed host-owned torana_meta_json")
	}

	acc := fingerprintRequestSections(accepted)
	res := fingerprintRequestSections(out)

	// Roles are compared over the union of both sides: a role that left a slot
	// and the role that took it are both marked changed, which is the
	// conservative reading of a reorder or replacement.
	for role, sum := range res.messages {
		if acc.messages[role] != sum {
			grant := string(sdk.MessageWriteSection(role))
			if !canWrite(grant) {
				return fmt.Errorf("plugin changed messages.%s without %s", role, grant)
			}
		}
	}
	for role, sum := range acc.messages {
		if res.messages[role] != sum {
			grant := string(sdk.MessageWriteSection(role))
			if !canWrite(grant) {
				return fmt.Errorf("plugin changed messages.%s without %s", role, grant)
			}
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

	return verifyRequestSignatures(accepted, out)
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

// verifyRequestSignatures applies the bound-signature rule to every message.
//
// Messages are paired positionally, matching the fingerprint. For each binding
// and each output message, the accepted token and covered content are compared
// against the output: boundContentChanged is decided over the binding's whole
// content-ref set (thinking_signature covers thinking + redacted_thinking,
// trailing_signature covers thinking + content, content_signature covers
// content). A message that appears in the output where the accepted request
// had none compares against absent values, so a token minted on an inserted
// message classifies as added.
func verifyRequestSignatures(accepted, out *pb.ChatRequest) error {
	for _, check := range requestSignatureFields {
		for i, om := range out.Messages {
			if om == nil {
				continue
			}
			var am *pb.Message
			if i < len(accepted.Messages) {
				am = accepted.Messages[i]
			}
			boundChanged := false
			for _, ref := range check.refs {
				if messageStringField(am, ref) != messageStringField(om, ref) {
					boundChanged = true
					break
				}
			}
			class := outboundpolicy.ClassifySignatureMutation(
				messageStringField(am, check.sig), messageStringField(om, check.sig), boundChanged)
			if !class.Allowed() {
				return fmt.Errorf("plugin %s signature %s", check.binding.SignatureField, class)
			}
		}
	}
	return nil
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
// grant, in which case no verification is needed: every section it could
// change is grantable to it. ir.stream.write is deliberately absent — it
// governs the stream topology path, not the request sections verified here.
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
