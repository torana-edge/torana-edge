package plugin

import (
	"fmt"
	"strings"

	"github.com/torana-edge/torana-plugin-sdk/outboundpolicy"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// Stream signature verification (Migration B, part 2a).
//
// verifyStream checks a plugin's returned StreamEvents against the accepted
// stream it was handed, on TWO axes only:
//
//  1. Bound provider signatures — the outbound `signature_delta` bindings
//     (SignatureScopeCurrentContentBlock and SignatureScopeTrailingStandalone)
//     and ToolCallRef.signature (SignatureScopeToolCallBlockByIndex), classified
//     transactionally over each binding's scope via
//     outboundpolicy.ClassifySignatureMutation. On the STREAM path there is no
//     apply block to invalidate a wire token before it ships, so SignatureStale
//     is a violation here (unlike the response path, which tolerates a stale
//     token because clearStaleSignatures normalizes it before it escapes).
//
//  2. The signed-block topology axis: suppressing a signed tool block or a
//     signed text/thinking block, or inventing a tool block, changes stream
//     cardinality and requires the topology grant ir.stream.write.
//
// EVERYTHING ELSE is deliberately out of scope for this PR: index uniqueness,
// open-block discipline, event-kind switches, text-inside-tool-block, Usage /
// MessageStart / MessageStop mutation, fan-out of unsigned content — the
// registry's full recursive field-policy walk lands with enforcement in the
// follow-up. This function exists to produce the numbers that gate turning
// that enforcement on, so it verifies signatures and the signed-block
// topology axis only, and says so.
//
// Signature comparison is TRANSACTIONAL over the binding's scope, never
// per-event: a tool block is compared as a whole (start id/name plus every
// arguments_delta sharing the index) between accepted and returned, so a
// buffering assembler that suppresses fragments and replays them
// byte-identically (StreamHandler's pass path) is intact, not forged.
//
// The signature_delta event-walk (scanStreamSignatures) is pinned by fixtures
// in streamverify_test.go; the two scopes are distinguished exactly as the
// SDK's SignatureScope comments demand:
//
//   - CurrentContentBlock: the signature_delta arrives while a text/thinking
//     span is open — an explicit ContentBlockStart{text|thinking} not yet
//     stopped, or, in the current host's boundary-less representation (the
//     engine IR has no text-block boundaries, so pbconv emits bare
//     TextDelta/ThinkingDelta), an implicit span of bare deltas. The covered
//     content is the deltas of that span up to the signature's position (the
//     provider's same-part text + thoughtSignature); the signature ends the
//     span, so a later part's text opens a fresh span.
//   - TrailingStandalone: the signature_delta arrives with NO open
//     text/thinking span (Code Assist's final {thoughtSignature,text:""}
//     part after closed blocks). It binds the preceding CLOSED text/thinking
//     content of the turn — the deltas of every span closed before it, which
//     for the real single-run shape equals "the deltas since the previous
//     ContentBlockStop". The KEY distinction from CurrentContentBlock is
//     whether closed content precedes: an open span (explicit or implicit)
//     means current; only a signature over no open span is trailing.
//   - A signature_delta with neither an open span nor any preceding closed
//     text/thinking content (e.g. after a tool-call-only turn) binds nothing:
//     the ABI requires a compatible open block for signature_delta and the
//     binding does not cover tool-call blocks, so it is rejected as
//     malformed. This is the conservative reading of the SDK contract
//     ("does not bind tool-call blocks") and is flagged for review.

// --- tool-call blocks (SignatureScopeToolCallBlockByIndex) -----------------

// toolCallScope is one tool-call block's signed scope. It mirrors the SDK's
// outboundpolicy scopeOf semantics exactly: id/name/signature from the
// ContentBlockStart{tool_call} at the index, arguments assembled from every
// ToolCallDelta.ArgumentsDelta sharing that index, in stream order. Fragment
// boundaries are transport and must not appear in the scope; deltas from
// other indexes must not be pooled in.
type toolCallScope struct {
	found     bool
	signature string
	id        string
	name      string
	arguments string
}

// toolCallScopeOf computes the scope for the tool-call block at index.
// Exposed (unexported) for the wrong-verifier discrimination tests, which
// build incorrect variants on top of it exactly as the SDK's
// TestWrongVerifiersFailTheFixtures does.
func toolCallScopeOf(events []*pbv2.StreamEvent, index int32) toolCallScope {
	var s toolCallScope
	for _, ev := range events {
		switch e := ev.Event.(type) {
		case *pbv2.StreamEvent_ContentBlockStart:
			cbs := e.ContentBlockStart
			if cbs.Index != index {
				continue
			}
			tc, ok := cbs.Block.(*pbv2.ContentBlockStart_ToolCall)
			if !ok {
				continue
			}
			s.found = true
			s.signature = tc.ToolCall.Signature
			s.id = tc.ToolCall.Id
			s.name = tc.ToolCall.Name
		case *pbv2.StreamEvent_ToolCallDelta:
			if e.ToolCallDelta.Index == index {
				s.arguments += e.ToolCallDelta.ArgumentsDelta
			}
		}
	}
	return s
}

// toolCallContentChanged reports whether everything the ToolCallRef.signature
// binding covers differs: id, name and the assembled arguments. Not the
// signature itself.
func toolCallContentChanged(a, b toolCallScope) bool {
	return a.id != b.id || a.name != b.name || a.arguments != b.arguments
}

// --- signature_delta bindings (CurrentContentBlock / TrailingStandalone) ----

// sigBindingKind names the scope a signature_delta occurrence was emitted in.
type sigBindingKind int

const (
	// sigBindingCurrent is a signature_delta inside an open text/thinking
	// span (explicit ContentBlockStart{text|thinking} or an implicit run of
	// bare deltas). Covers the span's deltas up to the signature.
	sigBindingCurrent sigBindingKind = iota
	// sigBindingTrailing is a signature_delta with no open text/thinking
	// span, after closed text/thinking content. Covers that closed content.
	sigBindingTrailing
	// sigBindingUnbound is a signature_delta with neither an open span nor
	// any preceding closed text/thinking content (a tool-call-only turn, a
	// leading signature, or one inside a tool/provider block). It binds
	// nothing and is rejected.
	sigBindingUnbound
)

func (k sigBindingKind) String() string {
	switch k {
	case sigBindingCurrent:
		return "current-block"
	case sigBindingTrailing:
		return "trailing"
	default:
		return "unbound"
	}
}

// sigBinding is one signature_delta occurrence with the scope the walk
// computed for it: the covered text/thinking content and the ordinal of the
// span it covered (CurrentContentBlock only; -1 for trailing/unbound).
type sigBinding struct {
	kind      sigBindingKind
	signature string
	content   string
	span      int
}

// streamSignatureView is the signature-relevant structure of one stream,
// produced by scanStreamSignatures.
type streamSignatureView struct {
	// toolScopes maps each tool-call block index to its assembled scope.
	toolScopes map[int32]toolCallScope
	// bindings lists the stream's signature_delta occurrences in stream order.
	bindings []sigBinding
	// spans lists the text/thinking span contents in the order they closed.
	// Used to tell a suppressed signed block (no span at the binding's
	// ordinal) from a cleared token over rewritten content (a different
	// span at that ordinal) and a dropped token over unchanged content (the
	// same span). Spans close at ContentBlockStop, at any ContentBlockStart
	// that is not a continuation, at a signature_delta (the signature ends
	// the span it covers), and at end of stream.
	spans []string
	// closedText is the concatenated content of every span closed so far —
	// the TrailingStandalone scope for a signature emitted at this point.
	closedText string
	// sawText reports whether any text/thinking delta was seen at all,
	// distinguishing "the signed content was suppressed" (nothing left to
	// compare) from "the signed content was rewritten" (cleared) when a
	// trailing token is gone and no closed text remains.
	sawText bool
}

// scanStreamSignatures walks one stream and computes every signature-relevant
// fact verifyStream needs: the per-index tool-call scopes and the
// signature_delta bindings with their scopes.
//
// The text/thinking span state machine:
//
//	ContentBlockStart{text|thinking}: any implicit run of bare deltas is
//	  closed; an explicit text/thinking block opens (its span starts on the
//	  first delta).
//	ContentBlockStart{tool_call|provider}: any implicit run is closed; a
//	  non-text block opens.
//	ContentBlockStop: the open span (explicit text block or implicit run) is
//	  closed.
//	TextDelta/ThinkingDelta with no open non-text block: appends to the
//	  current span, opening an implicit one when none is open. (A delta inside
//	  an open tool/provider block violates the ABI; it is ignored here — that
//	  discipline is the full walk's business, not this PR's.)
//	SignatureDelta: if a text/thinking span is open (explicit or implicit)
//	  → CurrentContentBlock binding over the span's deltas so far, then the
//	  span closes (the signature ends the provider part it came with). If a
//	  non-text block is open → unbound (malformed). Otherwise → trailing
//	  binding over the accumulated closed content, or unbound when no text/
//	  thinking content ever preceded it.
//
// A signature ending its span means a later TextDelta in the same explicit
// block opens a fresh span: each provider part's text is scoped separately,
// matching the SDK's "same provider part carried text/thinking and a
// thoughtSignature".
func scanStreamSignatures(events []*pbv2.StreamEvent) streamSignatureView {
	var v streamSignatureView
	var blockOpen, blockText bool
	var spanOpen bool
	var spanText strings.Builder
	var closed strings.Builder
	var toolIndexes []int32

	closeSpan := func() {
		if !spanOpen {
			return
		}
		closed.WriteString(spanText.String())
		v.spans = append(v.spans, spanText.String())
		spanText.Reset()
		spanOpen = false
	}

	for _, ev := range events {
		switch e := ev.Event.(type) {
		case *pbv2.StreamEvent_ContentBlockStart:
			closeSpan()
			cbs := e.ContentBlockStart
			blockOpen = true
			blockText = false
			switch cbs.Block.(type) {
			case *pbv2.ContentBlockStart_Text, *pbv2.ContentBlockStart_Thinking:
				blockText = true
			case *pbv2.ContentBlockStart_ToolCall:
				toolIndexes = append(toolIndexes, cbs.Index)
			}
		case *pbv2.StreamEvent_ContentBlockStop:
			closeSpan()
			blockOpen, blockText = false, false
		case *pbv2.StreamEvent_TextDelta:
			if blockOpen && !blockText {
				continue // ABI-violating text inside a tool/provider block; out of scope here
			}
			v.sawText = true
			spanText.WriteString(e.TextDelta)
			spanOpen = true
		case *pbv2.StreamEvent_ThinkingDelta:
			if blockOpen && !blockText {
				continue
			}
			v.sawText = true
			spanText.WriteString(e.ThinkingDelta)
			spanOpen = true
		case *pbv2.StreamEvent_SignatureDelta:
			switch {
			case spanOpen || (blockOpen && blockText):
				// Open text/thinking span: current-block binding over the
				// deltas since the span opened, up to this signature.
				v.bindings = append(v.bindings, sigBinding{
					kind:      sigBindingCurrent,
					signature: e.SignatureDelta,
					content:   spanText.String(),
					span:      len(v.spans),
				})
				closeSpan()
			case blockOpen:
				// Open tool/provider block: signature_delta is not the
				// tool-call path, so this binds nothing.
				v.bindings = append(v.bindings, sigBinding{
					kind:      sigBindingUnbound,
					signature: e.SignatureDelta,
					span:      -1,
				})
			default:
				// No open text/thinking span: trailing standalone over the
				// preceding closed content — or unbound when there was none.
				kind := sigBindingTrailing
				if closed.Len() == 0 && !v.sawText {
					kind = sigBindingUnbound
				}
				v.bindings = append(v.bindings, sigBinding{
					kind:      kind,
					signature: e.SignatureDelta,
					content:   closed.String(),
					span:      -1,
				})
			}
		}
	}
	closeSpan() // an implicit span still in flight at end of stream
	v.closedText = closed.String()

	if len(toolIndexes) > 0 {
		v.toolScopes = make(map[int32]toolCallScope, len(toolIndexes))
		for _, idx := range toolIndexes {
			if _, ok := v.toolScopes[idx]; !ok {
				v.toolScopes[idx] = toolCallScopeOf(events, idx)
			}
		}
	}
	return v
}

// --- verification -----------------------------------------------------------

// streamWriteGrant is the topology grant that authorises changing stream
// cardinality — suppressing a signed block or inventing a tool block.
const streamWriteGrant = "ir.stream.write"

// verifyStream checks a plugin's returned stream against the accepted one on
// the two axes above: bound signatures (tool blocks by index, signature_delta
// by scope) and the signed-block topology axis. It is pure — no pipeline or
// host access — so the enforcement wiring in the follow-up PR can call it
// with the plugin's grant lookup and reject on the first error.
//
// canWrite reports whether the plugin holds a named write grant; a nil
// canWrite is treated as holding none (verification never assumes grants).
func verifyStream(accepted, returned []*pbv2.StreamEvent, canWrite func(string) bool) error {
	if canWrite == nil {
		canWrite = func(string) bool { return false }
	}
	av := scanStreamSignatures(accepted)
	rv := scanStreamSignatures(returned)

	// Tool-call blocks, correlated by block index (the stable identity of a
	// tool block; the ABI requires unique, never-reused indexes).
	for idx, acc := range av.toolScopes {
		ret, ok := rv.toolScopes[idx]
		if !ok {
			// Accepted-only: the signed block was suppressed entirely. That
			// is a topology change; with the grant it is the recorded host
			// obligation, without it a plugin deleted a signed call.
			// Suppressing an UNSIGNED accepted block is pure topology — the
			// full field-policy walk's business, out of scope here.
			if acc.signature != "" && !canWrite(streamWriteGrant) {
				return fmt.Errorf("suppressed signed tool block %d without %s", idx, streamWriteGrant)
			}
			continue
		}
		class := outboundpolicy.ClassifySignatureMutation(acc.signature, ret.signature, toolCallContentChanged(acc, ret))
		if !class.Allowed() {
			return fmt.Errorf("tool block %d signature %s", idx, class)
		}
	}
	for idx := range rv.toolScopes {
		if _, ok := av.toolScopes[idx]; ok {
			continue
		}
		// Returned-only: a block the plugin invented. Changing stream
		// cardinality is the same topology gate as suppression.
		if !canWrite(streamWriteGrant) {
			return fmt.Errorf("invented tool block %d without %s", idx, streamWriteGrant)
		}
	}

	return verifySignatureDeltaBindings(av, rv, canWrite)
}

// verifySignatureDeltaBindings applies the bound-signature rule to every
// signature_delta occurrence, per scope, correlating the accepted and
// returned occurrences the way the request path correlates signed messages:
//
// Phase 1 — returned occurrences with a non-empty token consume one
// unconsumed accepted occurrence of the same scope with the SAME token value
// (a plugin cannot mint a provider token, so the value is the identity):
// exact content match is intact; a content mismatch with the token retained
// is stale (rejected); several candidates covering different content are
// ambiguous (rejected rather than guessed); no same-value candidate at all is
// forged when an unconsumed accepted occurrence of the scope remains, else
// added.
//
// Phase 2 — accepted occurrences no returned token covers (the token was
// suppressed, or re-emitted as an explicit empty signature_delta): an empty
// returned occurrence pairs positionally and ClassifySignatureMutation
// decides cleared/dropped by whether the covered content changed; otherwise
// the returned stream's span structure decides — the same content surviving
// is a dropped token (rejected), different content is the prescribed clearing
// (allowed), and a missing span at the binding's position means the signed
// block itself was suppressed (topology-gated).
func verifySignatureDeltaBindings(av, rv streamSignatureView, canWrite func(string) bool) error {
	for _, kind := range []sigBindingKind{sigBindingCurrent, sigBindingTrailing} {
		var acc, ret []sigBinding
		for _, b := range av.bindings {
			if b.kind == kind {
				acc = append(acc, b)
			}
		}
		for _, b := range rv.bindings {
			if b.kind == kind {
				ret = append(ret, b)
			}
		}

		consumed := make([]bool, len(acc))
		// Phase 1: returned non-empty tokens.
		for _, rb := range ret {
			if rb.signature == "" {
				continue
			}
			var candidates []int
			for i, ab := range acc {
				if !consumed[i] && ab.signature == rb.signature {
					candidates = append(candidates, i)
				}
			}
			if len(candidates) == 0 {
				unconsumed := 0
				for i := range acc {
					if !consumed[i] {
						unconsumed++
					}
				}
				if unconsumed > 0 {
					return fmt.Errorf("%s signature_delta forged", kind)
				}
				return fmt.Errorf("%s signature_delta added", kind)
			}
			first := candidates[0]
			for _, i := range candidates[1:] {
				if acc[i].content != acc[first].content {
					return fmt.Errorf("%s signature_delta ambiguous: %d signed occurrences cover different content", kind, len(candidates))
				}
			}
			consumed[first] = true
			if acc[first].content != rb.content {
				return fmt.Errorf("%s signature_delta stale", kind)
			}
		}

		// Phase 2: accepted occurrences the returned stream no longer signs.
		emptyRet := 0
		for _, rb := range ret {
			if rb.signature == "" {
				emptyRet++
			}
		}
		cleared := 0 // explicit empty returned occurrences consumed so far
		for i, ab := range acc {
			if consumed[i] || ab.signature == "" {
				continue
			}
			if cleared < emptyRet {
				// Explicit clear marker: empty signature_delta emitted in
				// the plugin's output at this scope position.
				class := outboundpolicy.ClassifySignatureMutation(ab.signature, "", ab.content != rv.emptyContent(kind, cleared))
				if !class.Allowed() {
					return fmt.Errorf("%s signature_delta %s", kind, class)
				}
				cleared++
				continue
			}
			if err := verifyUnpairedBinding(kind, ab, rv, canWrite); err != nil {
				return err
			}
		}
	}

	// Unbound signature_deltas (no covered content at all) are malformed on
	// either side: the ABI requires a compatible open block, the binding does
	// not cover tool-call blocks, and a floating token is a host-owned fact a
	// plugin could mint. Conservative reading, pinned by fixtures, flagged
	// for review.
	for _, b := range av.bindings {
		if b.kind == sigBindingUnbound {
			return fmt.Errorf("signature_delta has no covered content (does not bind tool-call blocks)")
		}
	}
	for _, b := range rv.bindings {
		if b.kind == sigBindingUnbound {
			return fmt.Errorf("signature_delta has no covered content (does not bind tool-call blocks)")
		}
	}
	return nil
}

// emptyContent returns the covered content of the k-th explicit empty
// signature_delta in the returned stream for the given scope — the content an
// explicit clear marker was emitted beside, used to tell dropped from cleared.
func (v streamSignatureView) emptyContent(kind sigBindingKind, k int) string {
	for _, b := range v.bindings {
		if b.kind == kind && b.signature == "" {
			if k == 0 {
				return b.content
			}
			k--
		}
	}
	return ""
}

// verifyUnpairedBinding decides what happened to an accepted signature whose
// token no longer appears in the returned stream, from the returned span
// structure alone:
//
//   - the same covered content survives → dropped (token stripped from
//     content the provider actually signed) → rejected;
//   - different content occupies the position → cleared (the prescribed
//     response to a legitimate rewrite) → allowed;
//   - nothing occupies the position (current-block) or the turn has no text
//     left at all (trailing) → the signed block itself was suppressed →
//     topology-gated on ir.stream.write.
func verifyUnpairedBinding(kind sigBindingKind, ab sigBinding, rv streamSignatureView, canWrite func(string) bool) error {
	if kind == sigBindingTrailing {
		if rv.closedText == ab.content {
			return fmt.Errorf("%s signature_delta dropped", kind)
		}
		if rv.closedText == "" && !rv.sawText {
			if !canWrite(streamWriteGrant) {
				return fmt.Errorf("suppressed a signed text/thinking block without %s", streamWriteGrant)
			}
			return nil
		}
		return nil // rewritten: the prescribed clearing
	}
	// CurrentContentBlock: the returned span at the binding's ordinal, or
	// the same content anywhere (re-framed spans are transport).
	if ab.span >= 0 && ab.span < len(rv.spans) && rv.spans[ab.span] == ab.content {
		return fmt.Errorf("%s signature_delta dropped", kind)
	}
	for _, s := range rv.spans {
		if s == ab.content {
			return fmt.Errorf("%s signature_delta dropped", kind)
		}
	}
	if ab.span >= 0 && ab.span >= len(rv.spans) {
		if !canWrite(streamWriteGrant) {
			return fmt.Errorf("suppressed a signed text/thinking block without %s", streamWriteGrant)
		}
		return nil
	}
	return nil // a different span sits at the position: cleared
}
