package plugin

import (
	"fmt"
	"strings"

	"github.com/torana-edge/torana-plugin-sdk/outboundpolicy"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// Stream signature verification (Migration B, part 2a; reworked per the
// #243 round-1 findings).
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
//     cardinality and requires the topology grant ir.stream.write — with the
//     one hard exception that an INVENTED SIGNED tool block is a minted
//     signature (added) and is rejected even with every grant.
//
// EVERYTHING ELSE is deliberately out of scope for this PR: index uniqueness,
// open-block discipline on the plugin's OUTPUT, event-kind switches in the
// output, Usage / MessageStart / MessageStop mutation, fan-out of unsigned
// content — the registry's full recursive field-policy walk lands with
// enforcement in the follow-up (2b). This function exists to produce the
// numbers that gate turning that enforcement on, so it verifies signatures
// and the signed-block topology axis only, and says so.
//
// The accepted/plugin split (round-1 decision 1): a malformed ACCEPTED stream
// is a host/adaptor defect, NOT a plugin failure — the plugin's output can
// only be judged against a valid baseline. validateAcceptedStream checks the
// host side (unbound signature_delta, incompatible deltas, missing stops,
// events after StreamError) and returns its findings as *acceptedStreamError;
// verifyStream calls it defensively on the accepted events and propagates the
// typed error, so enforcement can tell "the host handed the plugin garbage"
// from "the plugin violated the contract". A malformed signature_delta in the
// plugin's OUTPUT is a plugin violation.
//
// Signature comparison is TRANSACTIONAL over the binding's scope, never
// per-event: a tool block is compared as a whole (start id/name plus every
// arguments_delta sharing the index) between accepted and returned, so a
// buffering assembler that suppresses fragments and replays them
// byte-identically (StreamHandler's pass path) is intact, not forged.
//
// Scope representation (round-1 finding): a text/thinking scope is a TYPED
// record — separate text and thinking accumulators — so identical bytes in
// different kinds differ. A signature over text "A" is NOT the same scope as
// a signature over thinking "A"; carrying a token across the kind boundary is
// stale.
//
// Correlation (round-1 findings): exact matches are found and consumed FIRST
// — a returned occurrence pairs with an unconsumed accepted occurrence of the
// same scope with the same token value AND identical typed content — and
// ambiguity/stale are only diagnosed once no exact match remains. Two signed
// spans sharing one token value pass through unchanged instead of being
// misread as ambiguous. Tool blocks are assembled by index WITHIN each side
// and aligned one-to-one by their complete signed facts ACROSS sides: a
// coherent block reindex with all facts (id, name, arguments, token) moving
// together is NOT forged at this layer — the index movement is a topology
// change whose charge belongs to 2b's full walk. A token that did NOT move
// with its block's facts (token-detached reindex) is stale and IS caught.
//
// The event walk is a single O(n) pass per stream: tool scopes are built in
// the main walk, one builder per OPEN tool block (concurrent blocks keep
// separate accumulators keyed by index), and materialized once at their stop —
// no rescanning of the event slice, no per-index re-assembly.

// acceptedStreamError marks a defect in the ACCEPTED stream: a host/adaptor
// bug, never a plugin failure. validateAcceptedStream returns only errors of
// this type, and verifyStream propagates them unchanged, so a caller can
// distinguish "the accepted stream the host dispatched was malformed" from
// "the plugin violated the contract" with errors.As.
type acceptedStreamError struct{ msg string }

func (e *acceptedStreamError) Error() string { return "accepted stream: " + e.msg }

// validateAcceptedStream validates the ACCEPTED event sequence a host hands a
// plugin, before dispatch. Everything here is host discipline — failing it is
// an adapter defect, not a plugin failure — so the checks deliberately live
// apart from verifyStream's plugin-violation checks:
//
//   - unbound signature_delta: a signature_delta with no covered content (no
//     open text/thinking span, no open text/thinking block, and no preceding
//     closed text/thinking content). The ABI requires a compatible open block
//     and the binding does not cover tool-call blocks; a floating token the
//     plugin is then free to mirror is a host-owned fact with no definition.
//   - incompatible deltas: a text/thinking delta inside a block of the other
//     kind or inside an open tool/provider block, or a tool-call delta / stop
//     naming no open block of the matching kind. (A text delta inside a
//     thinking block is the pinned case.)
//   - missing stop at successful completion: the stream ends with an explicit
//     content block still open and no StreamError to abandon it. Implicit
//     bare-delta spans are the boundary-less host representation and need no
//     stop.
//   - events after StreamError: StreamError is terminal; anything after it is
//     malformed.
//
// The walk is a single O(n) pass and mirrors scanStreamSignatures' span state
// machine exactly (span closes at any block start, at a non-tool stop, and at
// a signature_delta), so the current/trailing/unbound verdict agrees with the
// scope walk that verifyStream runs: the unbound test here is the negation of
// "current or trailing" as scanStreamSignatures defines it.
// openBlockKind names the kind of the single open non-tool block during
// accepted-stream validation. Non-tool blocks are exclusive (only TOOL blocks
// may be concurrent), so one slot suffices.
type openBlockKind int

const (
	noBlock openBlockKind = iota
	textBlockOpen
	thinkingBlockOpen
	providerBlockOpen
)

func (k openBlockKind) String() string {
	switch k {
	case textBlockOpen:
		return "text"
	case thinkingBlockOpen:
		return "thinking"
	case providerBlockOpen:
		return "provider"
	}
	return "none"
}

func validateAcceptedStream(events []*pbv2.StreamEvent) error {
	var block openBlockKind
	var openTools = make(map[int32]bool)
	var sawError bool
	var spanOpen bool // a text/thinking span is in flight (explicit or implicit)
	var sawText bool  // any text/thinking delta was seen at all

	for i, ev := range events {
		if sawError {
			return &acceptedStreamError{msg: fmt.Sprintf("event after StreamError at position %d", i)}
		}
		switch e := ev.Event.(type) {
		case *pbv2.StreamEvent_Error:
			sawError = true
		case *pbv2.StreamEvent_ContentBlockStart:
			spanOpen = false // any start closes an implicit run, as in the scope walk
			switch e.ContentBlockStart.Block.(type) {
			case *pbv2.ContentBlockStart_Text:
				block = textBlockOpen
			case *pbv2.ContentBlockStart_Thinking:
				block = thinkingBlockOpen
			case *pbv2.ContentBlockStart_ToolCall:
				openTools[e.ContentBlockStart.Index] = true
			case *pbv2.ContentBlockStart_Provider:
				block = providerBlockOpen
			}
		case *pbv2.StreamEvent_ContentBlockStop:
			if openTools[e.ContentBlockStop.Index] {
				delete(openTools, e.ContentBlockStop.Index)
				continue
			}
			if block == noBlock {
				return &acceptedStreamError{msg: fmt.Sprintf("content block stop at position %d names no open block", i)}
			}
			block = noBlock
			spanOpen = false
		case *pbv2.StreamEvent_TextDelta:
			if block == thinkingBlockOpen || block == providerBlockOpen {
				return &acceptedStreamError{msg: fmt.Sprintf("text delta at position %d inside a %s block", i, block)}
			}
			if len(openTools) > 0 {
				return &acceptedStreamError{msg: fmt.Sprintf("text delta at position %d while a tool block is open", i)}
			}
			spanOpen, sawText = true, true
		case *pbv2.StreamEvent_ThinkingDelta:
			if block == textBlockOpen || block == providerBlockOpen {
				return &acceptedStreamError{msg: fmt.Sprintf("thinking delta at position %d inside a %s block", i, block)}
			}
			if len(openTools) > 0 {
				return &acceptedStreamError{msg: fmt.Sprintf("thinking delta at position %d while a tool block is open", i)}
			}
			spanOpen, sawText = true, true
		case *pbv2.StreamEvent_SignatureDelta:
			// Mirrors the scope walk: an open span or an open text/thinking
			// block is current (the signature ends the span); a signature
			// inside an open tool/provider block, or one with no text/thinking
			// content anywhere, binds nothing.
			if spanOpen || block == textBlockOpen || block == thinkingBlockOpen {
				spanOpen = false
				continue
			}
			if block == providerBlockOpen || len(openTools) > 0 || !sawText {
				return &acceptedStreamError{msg: "signature_delta has no covered content (does not bind tool-call blocks)"}
			}
		case *pbv2.StreamEvent_ToolCallDelta:
			if !openTools[e.ToolCallDelta.Index] {
				return &acceptedStreamError{msg: fmt.Sprintf("tool call delta at position %d names no open tool block (%d)", i, e.ToolCallDelta.Index)}
			}
		}
	}

	if !sawError && (block != noBlock || len(openTools) > 0) {
		return &acceptedStreamError{msg: "stream ends with an open content block; missing ContentBlockStop (or StreamError)"}
	}
	return nil
}

// --- typed text/thinking content -------------------------------------------

// typedContent is the content of a text/thinking span as a TYPED record:
// separate text and thinking accumulators, so identical bytes in different
// kinds differ (round-1 finding: a scope is a typed record — a signature over
// text "A" must not match a signature over thinking "A").
type typedContent struct {
	text     string
	thinking string
}

func (t typedContent) equals(o typedContent) bool {
	return t.text == o.text && t.thinking == o.thinking
}

func (t typedContent) isEmpty() bool { return t.text == "" && t.thinking == "" }

// --- tool-call blocks (SignatureScopeToolCallBlockByIndex) -----------------

// toolCallScope is one tool-call block's signed scope. It mirrors the SDK's
// outboundpolicy scopeOf semantics exactly: id/name/signature from the
// ContentBlockStart{tool_call} at the index, arguments assembled from every
// ToolCallDelta.ArgumentsDelta sharing that index, in stream order. Fragment
// boundaries are transport and must not appear in the scope; deltas from
// other indexes must not be pooled in.
type toolCallScope struct {
	index     int32
	found     bool
	signature string
	id        string
	name      string
	arguments string
}

// toolCallScopeOf computes the scope for the tool-call block at index by
// rescanning the event slice. This is TEST-FACING ONLY: production scope
// assembly happens in the single-pass walk (scanStreamSignatures) with one
// builder per open block. Exposed (unexported) for the wrong-verifier
// discrimination tests, which build incorrect variants on top of it exactly
// as the SDK's TestWrongVerifiersFailTheFixtures does.
func toolCallScopeOf(events []*pbv2.StreamEvent, index int32) toolCallScope {
	var s toolCallScope
	s.index = index
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

// toolScopeBuilder accumulates one OPEN tool block's scope during the single
// pass. One builder per open block — concurrent tool blocks keep separate
// accumulators keyed by index — materialized exactly once at the block's stop.
type toolScopeBuilder struct {
	index     int32
	signature string
	id        string
	name      string
	args      strings.Builder
}

func (b *toolScopeBuilder) scope() toolCallScope {
	return toolCallScope{
		index:     b.index,
		found:     true,
		signature: b.signature,
		id:        b.id,
		name:      b.name,
		arguments: b.args.String(),
	}
}

// --- signature_delta bindings (CurrentContentBlock / TrailingStandalone) ----

// sigBindingKind names the scope a signature_delta occurrence was emitted in.
type sigBindingKind int

const (
	// sigBindingCurrent is a signature_delta inside an open text/thinking
	// span (explicit ContentBlockStart{text|thinking} or an implicit run of
	// bare deltas). Covers the span's typed deltas up to the signature.
	sigBindingCurrent sigBindingKind = iota
	// sigBindingTrailing is a signature_delta with no open text/thinking
	// span, after closed text/thinking content. Covers that closed content.
	sigBindingTrailing
	// sigBindingUnbound is a signature_delta with neither an open span nor
	// any preceding closed text/thinking content (a tool-call-only turn, a
	// leading signature, or one inside a tool/provider block). It binds
	// nothing. In ACCEPTED input it is a host defect
	// (validateAcceptedStream); in plugin OUTPUT it is a violation.
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
// computed for it: the covered typed content and the ordinal of the span it
// covered (CurrentContentBlock only; -1 for trailing/unbound).
type sigBinding struct {
	kind      sigBindingKind
	signature string
	content   typedContent
	span      int
}

// streamSignatureView is the signature-relevant structure of one stream,
// produced by scanStreamSignatures.
type streamSignatureView struct {
	// toolScopes lists the assembled tool-call scopes in stream order (each
	// materialized once, at its block's stop).
	toolScopes []toolCallScope
	// bindings lists the stream's signature_delta occurrences in stream order.
	bindings []sigBinding
	// spans lists the typed text/thinking span contents in the order they
	// closed. Used to tell a suppressed signed block (no span at the
	// binding's ordinal) from a cleared token over rewritten content (a
	// different span at that ordinal) and a dropped token over unchanged
	// content (the same span). Spans close at ContentBlockStop, at any
	// ContentBlockStart that is not a continuation, at a signature_delta
	// (the signature ends the span it covers), and at end of stream.
	spans []typedContent
	// closed is the concatenated typed content of every span closed so far —
	// the TrailingStandalone scope for a signature emitted at this point.
	closed typedContent
	// sawContent reports whether any text/thinking delta was seen at all,
	// distinguishing "the signed content was suppressed" (nothing left to
	// compare) from "the signed content was rewritten" (cleared) when a
	// trailing token is gone and no closed text remains.
	sawContent bool
}

// scanStreamSignatures walks one stream ONCE and computes every
// signature-relevant fact verifyStream needs: the per-index tool-call scopes
// (built inline, one builder per OPEN tool block — concurrent blocks keep
// separate accumulators keyed by index — and materialized once at their stop)
// and the signature_delta bindings with their typed scopes. No rescanning:
// this is the O(n) walk.
//
// The typed text/thinking span state machine:
//
//	ContentBlockStart{text|thinking}: any implicit run of bare deltas is
//	  closed; an explicit text/thinking block opens (its span starts on the
//	  first delta).
//	ContentBlockStart{tool_call}: a per-index tool builder opens; the
//	  non-tool state is untouched (tool blocks may be concurrent).
//	ContentBlockStart{provider}: any implicit run is closed; a non-text
//	  block opens.
//	ContentBlockStop: a stop naming an open tool block materializes and
//	  closes that tool scope; otherwise the open span (explicit text block or
//	  implicit run) is closed.
//	TextDelta/ThinkingDelta with no open non-text block and no open tool
//	  block: appends to the span's typed accumulator (text or thinking),
//	  opening an implicit span when none is open. (A delta inside an open
//	  tool/provider block, or of the wrong kind for an open text/thinking
//	  block, violates the ABI; it is ignored here — that discipline is
//	  validateAcceptedStream's on the accepted side and the full walk's on
//	  the plugin-output side, not this PR's.)
//	SignatureDelta: if a text/thinking span is open (explicit or implicit)
//	  → CurrentContentBlock binding over the span's typed deltas so far, then
//	  the span closes (the signature ends the provider part it came with). If
//	  a non-text block or any tool block is open → unbound (malformed).
//	  Otherwise → trailing binding over the accumulated closed content, or
//	  unbound when no text/thinking content ever preceded it.
//
// A signature ending its span means a later TextDelta in the same explicit
// block opens a fresh span: each provider part's text is scoped separately,
// matching the SDK's "same provider part carried text/thinking and a
// thoughtSignature".
func scanStreamSignatures(events []*pbv2.StreamEvent) streamSignatureView {
	var v streamSignatureView
	var blockOpen, blockText bool
	var spanOpen bool
	var spanText, spanThink strings.Builder
	var closedText, closedThink strings.Builder
	var sawText bool
	openTools := make(map[int32]*toolScopeBuilder)

	closeSpan := func() {
		if !spanOpen {
			return
		}
		tc := typedContent{text: spanText.String(), thinking: spanThink.String()}
		closedText.WriteString(tc.text)
		closedThink.WriteString(tc.thinking)
		v.spans = append(v.spans, tc)
		spanText.Reset()
		spanThink.Reset()
		spanOpen = false
	}

	for _, ev := range events {
		switch e := ev.Event.(type) {
		case *pbv2.StreamEvent_ContentBlockStart:
			closeSpan()
			cbs := e.ContentBlockStart
			switch cbs.Block.(type) {
			case *pbv2.ContentBlockStart_Text, *pbv2.ContentBlockStart_Thinking:
				blockOpen, blockText = true, true
			case *pbv2.ContentBlockStart_ToolCall:
				if _, ok := openTools[cbs.Index]; !ok {
					b := &toolScopeBuilder{index: cbs.Index}
					tc := cbs.Block.(*pbv2.ContentBlockStart_ToolCall).ToolCall
					b.signature, b.id, b.name = tc.Signature, tc.Id, tc.Name
					openTools[cbs.Index] = b
				}
			case *pbv2.ContentBlockStart_Provider:
				blockOpen, blockText = true, false
			}
		case *pbv2.StreamEvent_ContentBlockStop:
			if b, ok := openTools[e.ContentBlockStop.Index]; ok {
				delete(openTools, e.ContentBlockStop.Index)
				v.toolScopes = append(v.toolScopes, b.scope())
				continue
			}
			closeSpan()
			blockOpen, blockText = false, false
		case *pbv2.StreamEvent_ToolCallDelta:
			if b, ok := openTools[e.ToolCallDelta.Index]; ok {
				b.args.WriteString(e.ToolCallDelta.ArgumentsDelta)
			}
		case *pbv2.StreamEvent_TextDelta:
			if (blockOpen && !blockText) || len(openTools) > 0 {
				continue // ABI-violating text; charged by the strict walk
			}
			sawText = true
			spanText.WriteString(e.TextDelta)
			spanOpen = true
		case *pbv2.StreamEvent_ThinkingDelta:
			if (blockOpen && !blockText) || len(openTools) > 0 {
				continue // ABI-violating thinking; charged by the strict walk
			}
			sawText = true
			spanThink.WriteString(e.ThinkingDelta)
			spanOpen = true
		case *pbv2.StreamEvent_SignatureDelta:
			switch {
			case spanOpen || (blockOpen && blockText):
				// Open text/thinking span: current-block binding over the
				// span's typed deltas up to this signature.
				v.bindings = append(v.bindings, sigBinding{
					kind:      sigBindingCurrent,
					signature: e.SignatureDelta,
					content:   typedContent{text: spanText.String(), thinking: spanThink.String()},
					span:      len(v.spans),
				})
				closeSpan()
			case blockOpen || len(openTools) > 0:
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
				if closedText.Len() == 0 && closedThink.Len() == 0 && !sawText {
					kind = sigBindingUnbound
				}
				v.bindings = append(v.bindings, sigBinding{
					kind:      kind,
					signature: e.SignatureDelta,
					content:   typedContent{text: closedText.String(), thinking: closedThink.String()},
					span:      -1,
				})
			}
		}
	}
	closeSpan() // an implicit span still in flight at end of stream
	v.closed = typedContent{text: closedText.String(), thinking: closedThink.String()}
	v.sawContent = sawText
	return v
}

// --- verification -----------------------------------------------------------

// streamWriteGrant is the topology grant that authorises changing stream
// cardinality — suppressing a signed block or inventing a tool block.
const streamWriteGrant = "ir.stream.write"

// verifyStream checks a plugin's returned stream against the accepted one on
// the two axes above: bound signatures (tool blocks by index, signature_delta
// by typed scope) and the signed-block topology axis. It is pure — no
// pipeline or host access — so the enforcement wiring in the follow-up PR can
// call it with the plugin's grant lookup and reject on the first error.
//
// A malformed ACCEPTED stream is a host/adaptor defect, not a plugin failure:
// verifyStream validates the accepted side first (validateAcceptedStream) and
// propagates its *acceptedStreamError unchanged, so a caller can tell the two
// apart with errors.As. Plugin-output discipline beyond signatures (index
// uniqueness, open-block discipline, event-kind switches in the output, …) is
// the full walk's business and deliberately not checked here.
//
// canWrite reports whether the plugin holds a named write grant; a nil
// canWrite is treated as holding none (verification never assumes grants).
func verifyStream(accepted, returned []*pbv2.StreamEvent, canWrite func(string) bool) error {
	if canWrite == nil {
		canWrite = func(string) bool { return false }
	}
	if err := validateAcceptedStream(accepted); err != nil {
		return err
	}
	av := scanStreamSignatures(accepted)
	rv := scanStreamSignatures(returned)

	if err := verifyToolScopes(av.toolScopes, rv.toolScopes, canWrite); err != nil {
		return err
	}
	return verifySignatureDeltaBindings(av, rv, canWrite)
}

// verifyToolScopes applies the bound-signature rule to every tool-call block.
//
// Scopes are assembled by index WITHIN each side (the walk does this); the
// ACROSS-sides alignment is one-to-one by the complete signed facts, not by
// index (round-1 reindex finding):
//
// Phase 1 — a returned scope that reproduces an accepted scope's complete
// facts (signature, id, name, arguments) at ANY index is intact and consumes
// it. Pass-through AND a coherent block reindex (all facts moving together
// across indexes) both land here: a reindex is NOT forged — the index
// movement is a topology change whose charge belongs to 2b's full walk.
//
// Phase 2 — a returned scope that is not an exact repeat is correlated with
// the accepted scope at the SAME index (if one is unconsumed) via
// ClassifySignatureMutation, which decides cleared/dropped/stale/forged/added
// from the token change and the scope diff. A token that did NOT move with
// its block's facts (token-detached reindex) is caught here as stale.
//
// Phase 3 — a returned scope with no accepted counterpart at its index is
// invented: a SIGNED invented block is a minted signature (added) and is
// rejected regardless of grants (round-1 decision 2: the signature verifier
// is the single implementation of bound-signature semantics); an UNSIGNED
// invented block is a cardinality change and needs ir.stream.write.
//
// Phase 4 — accepted scopes no returned scope consumed are suppressed: a
// SIGNED block's disappearance is the recorded host obligation and is
// topology-gated; an UNSIGNED block's suppression is pure topology, the full
// field-policy walk's business, and passes here by design.
func verifyToolScopes(accepted, returned []toolCallScope, canWrite func(string) bool) error {
	consumed := make([]bool, len(accepted))

	for _, r := range returned {
		// Phase 1: exact facts, any index.
		matched := -1
		for i := range accepted {
			if !consumed[i] &&
				accepted[i].signature == r.signature &&
				accepted[i].id == r.id &&
				accepted[i].name == r.name &&
				accepted[i].arguments == r.arguments {
				matched = i
				break
			}
		}
		if matched >= 0 {
			consumed[matched] = true
			continue
		}

		// Phase 2: same-index correlation.
		ai := -1
		for i := range accepted {
			if accepted[i].index == r.index {
				ai = i
				break
			}
		}
		if ai >= 0 && !consumed[ai] {
			class := outboundpolicy.ClassifySignatureMutation(
				accepted[ai].signature, r.signature, toolCallContentChanged(accepted[ai], r))
			if !class.Allowed() {
				return fmt.Errorf("tool block %d signature %s", r.index, class)
			}
			consumed[ai] = true
			continue
		}

		// Phase 3: invented block.
		if r.signature != "" {
			return fmt.Errorf("invented signed tool block %d: signature added", r.index)
		}
		if !canWrite(streamWriteGrant) {
			return fmt.Errorf("invented tool block %d without %s", r.index, streamWriteGrant)
		}
	}

	// Phase 4: suppressed accepted blocks.
	for i := range accepted {
		if consumed[i] || accepted[i].signature == "" {
			continue
		}
		if !canWrite(streamWriteGrant) {
			return fmt.Errorf("suppressed signed tool block %d without %s", accepted[i].index, streamWriteGrant)
		}
	}
	return nil
}

// verifySignatureDeltaBindings applies the bound-signature rule to every
// signature_delta occurrence, per scope, correlating the accepted and
// returned occurrences by exact (token, typed-content) matches FIRST (round-1
// finding: ambiguity and stale are only diagnosed once no exact unconsumed
// occurrence exists — repeated token values over different content pass
// through unchanged instead of being misread as ambiguous):
//
// Phase 1 — every returned occurrence with a non-empty token consumes one
// unconsumed accepted occurrence of the same scope with the SAME token value
// AND identical typed content (intact; a plugin cannot mint a provider token,
// so the token value plus the content it covers is the identity).
//
// Phase 2 — returned occurrences with no exact match left: an unconsumed
// accepted occurrence with the same token value but different content is
// stale (the token survived over content the provider never signed; several
// candidates covering different content are ambiguous, rejected rather than
// guessed); no same-token candidate at all is forged when an unconsumed
// accepted occurrence of the scope remains, else added.
//
// Phase 3 — accepted occurrences no returned token covers: an explicit empty
// returned occurrence whose typed content exactly matches is a dropped token
// (explicitly emptied over content the provider signed — rejected); otherwise
// empty returned occurrences pair positionally and clear (the prescribed
// response to a legitimate rewrite). What remains is decided from the returned
// span structure by verifyUnpairedBinding: the same content surviving is a
// dropped token (rejected), different content is clearing (allowed), and a
// missing span at the binding's position means the signed block itself was
// suppressed (topology-gated).
//
// An UNBOUND signature_delta in the plugin's OUTPUT is a violation (a
// floating token the plugin could mint); in ACCEPTED input it is a host
// defect already rejected by validateAcceptedStream.
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
		retConsumed := make([]bool, len(ret))

		// Phase 1: exact (token, typed-content) matches first.
		for j := range ret {
			if ret[j].signature == "" {
				continue
			}
			for i := range acc {
				if !consumed[i] && acc[i].signature == ret[j].signature &&
					acc[i].content.equals(ret[j].content) {
					consumed[i] = true
					retConsumed[j] = true
					break
				}
			}
		}

		// Phase 2: returned occurrences with no exact match.
		for j := range ret {
			if ret[j].signature == "" || retConsumed[j] {
				continue
			}
			var candidates []int
			for i := range acc {
				if !consumed[i] && acc[i].signature == ret[j].signature {
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
				if !acc[i].content.equals(acc[first].content) {
					return fmt.Errorf("%s signature_delta ambiguous: %d signed occurrences cover different content", kind, len(candidates))
				}
			}
			consumed[first] = true
			return fmt.Errorf("%s signature_delta stale", kind)
		}

		// Phase 3: accepted occurrences the returned stream no longer signs.
		var emptyRet []int // indexes into ret of explicit empty occurrences
		for j := range ret {
			if ret[j].signature == "" {
				emptyRet = append(emptyRet, j)
			}
		}
		emptyUsed := make([]bool, len(emptyRet))
		exactEmpty := func(c typedContent) int {
			for k, j := range emptyRet {
				if !emptyUsed[k] && ret[j].content.equals(c) {
					return k
				}
			}
			return -1
		}
		for i := range acc {
			if consumed[i] || acc[i].signature == "" {
				continue
			}
			// Exact empty-marker match: the plugin emitted an explicit empty
			// signature_delta beside the SAME typed content — the token was
			// stripped from content the provider actually signed.
			if k := exactEmpty(acc[i].content); k >= 0 {
				emptyUsed[k] = true
				consumed[i] = true
				return fmt.Errorf("%s signature_delta dropped", kind)
			}
			// Positional cleared pairing: an unused empty marker clears this
			// accepted occurrence (content was rewritten).
			cleared := 0
			for cleared < len(emptyUsed) && emptyUsed[cleared] {
				cleared++
			}
			if cleared < len(emptyUsed) {
				emptyUsed[cleared] = true
				consumed[i] = true
				continue
			}
			if err := verifyUnpairedBinding(kind, acc[i], rv, canWrite); err != nil {
				return err
			}
		}
	}

	// Unbound signature_delta in the plugin's OUTPUT is a violation: the ABI
	// requires a compatible open block, the binding does not cover tool-call
	// blocks, and a floating token is a host-owned fact a plugin could mint.
	for _, b := range rv.bindings {
		if b.kind == sigBindingUnbound {
			return fmt.Errorf("signature_delta has no covered content (does not bind tool-call blocks)")
		}
	}
	return nil
}

// verifyUnpairedBinding decides what happened to an accepted signature whose
// token no longer appears in the returned stream, from the returned span
// structure alone:
//
//   - the same typed content survives → dropped (token stripped from content
//     the provider actually signed) → rejected;
//   - different content occupies the position → cleared (the prescribed
//     response to a legitimate rewrite) → allowed;
//   - nothing occupies the position (current-block) or the turn has no text
//     left at all (trailing) → the signed block itself was suppressed →
//     topology-gated on ir.stream.write.
func verifyUnpairedBinding(kind sigBindingKind, ab sigBinding, rv streamSignatureView, canWrite func(string) bool) error {
	if kind == sigBindingTrailing {
		if rv.closed.equals(ab.content) {
			return fmt.Errorf("trailing signature_delta dropped")
		}
		if rv.closed.isEmpty() && !rv.sawContent {
			if !canWrite(streamWriteGrant) {
				return fmt.Errorf("suppressed a signed text/thinking block without %s", streamWriteGrant)
			}
			return nil
		}
		return nil // rewritten: the prescribed clearing
	}
	// CurrentContentBlock: the returned span at the binding's ordinal, or
	// the same typed content anywhere (re-framed spans are transport).
	if ab.span >= 0 && ab.span < len(rv.spans) && rv.spans[ab.span].equals(ab.content) {
		return fmt.Errorf("current-block signature_delta dropped")
	}
	for _, s := range rv.spans {
		if s.equals(ab.content) {
			return fmt.Errorf("current-block signature_delta dropped")
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
