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
// (Round-2 F1) Reindexing is topology, but it must not erase signature
// correlation: a returned scope whose covered facts (id, name, assembled
// arguments) reproduce an accepted scope one-to-one at a DIFFERENT index is
// still that block, and its token is classified over the unchanged content —
// stripped is dropped, replaced is forged, and both are rejected regardless
// of grants.
//
// (Round-2 F3) An explicit text/thinking block opened and closed with zero
// deltas is a span with EMPTY typed content: the block exists even without
// content, so an empty signed block whose token disappears is a dropped
// signature — never a suppressed block, which is what absence from the span
// list would otherwise claim. (Round-3 F1) The empty span is materialized AT
// THE SIGNATURE EVENT when a current-block signature is emitted in an
// explicit text/thinking block with no deltas yet, so later deltas become
// the next ordinal instead of rewriting over the empty signed scope.
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
//   - the FULL ABI topology (round-2 F4): the state is openNonTool{index,
//     kind} + openTools[index] + seen[index] — every stop/delta must name an
//     open block of the matching kind (a non-tool stop binds the open
//     non-tool block BY INDEX), non-tool blocks are exclusive and never
//     overlap tool blocks, indexes are never reused, MessageStop with ANY
//     open block is rejected immediately, and after MessageStop only Usage
//     (and a terminal StreamError) may follow — never a content/block event.
//   - missing stop at successful completion: the stream ends with an explicit
//     content block still open and no StreamError to abandon it. Implicit
//     bare-delta spans are the boundary-less host representation and need no
//     stop.
//   - events after StreamError: StreamError is terminal; anything after it is
//     malformed.
//
// StreamError remains the ONLY terminal path that abandons open blocks: a
// missing stop is invalid unless the stream already ended at a StreamError.
//
// The walk is a single O(n) pass and mirrors scanStreamSignatures' span state
// machine exactly (span closes at any block start, at a non-tool stop, and at
// a signature_delta), so the current/trailing/unbound verdict agrees with the
// scope walk that verifyStream runs: the unbound test here is the negation of
// "current or trailing" as scanStreamSignatures defines it.
// openBlockKind names the kind of the single open non-tool block during
// accepted-stream validation.
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

// openNonTool is the single open non-tool content block during accepted-stream
// validation, with its index AND kind. Non-tool blocks are exclusive (only
// TOOL blocks may be concurrent), so one slot suffices — but the slot must
// carry the index: a non-tool stop names the open non-tool block BY INDEX, and
// accepting a stop at any other index would validate a wrong close (round-2
// F4: StartText(3) + Stop(9) must not pass).
type openNonTool struct {
	index int32
	kind  openBlockKind
}

func validateAcceptedStream(events []*pbv2.StreamEvent) error {
	var nonTool *openNonTool
	var openTools = make(map[int32]bool)
	var seen = make(map[int32]bool) // every started index, tool or not
	var sawError bool
	var messageStopped bool
	var spanOpen bool // a text/thinking span is in flight (explicit or implicit)
	var sawText bool  // any text/thinking delta was seen at all

	for i, ev := range events {
		if sawError {
			return &acceptedStreamError{msg: fmt.Sprintf("event after StreamError at position %d", i)}
		}
		if messageStopped {
			// Per the ABI, Usage may still arrive after MessageStop (providers
			// differ on where it lands); StreamError is terminal at any point.
			// No other content/block event may follow MessageStop.
			switch ev.Event.(type) {
			case *pbv2.StreamEvent_Usage, *pbv2.StreamEvent_Error:
			default:
				return &acceptedStreamError{msg: fmt.Sprintf("event at position %d after MessageStop", i)}
			}
		}
		switch e := ev.Event.(type) {
		case *pbv2.StreamEvent_Error:
			sawError = true
		case *pbv2.StreamEvent_Usage:
			// No stream state to enforce for Usage.
		case *pbv2.StreamEvent_MessageStart:
			// Message framing; nothing to enforce before MessageStop.
		case *pbv2.StreamEvent_MessageStop:
			if nonTool != nil || len(openTools) > 0 {
				return &acceptedStreamError{msg: fmt.Sprintf("MessageStop at position %d while a content block is still open", i)}
			}
			messageStopped = true
		case *pbv2.StreamEvent_ContentBlockStart:
			spanOpen = false // any start closes an implicit run, as in the scope walk
			idx := e.ContentBlockStart.Index
			if seen[idx] {
				return &acceptedStreamError{msg: fmt.Sprintf("content block index %d reused at position %d", idx, i)}
			}
			seen[idx] = true
			switch e.ContentBlockStart.Block.(type) {
			case *pbv2.ContentBlockStart_Text:
				if nonTool != nil {
					return &acceptedStreamError{msg: fmt.Sprintf("non-tool content block start at position %d while a %s block is open", i, nonTool.kind)}
				}
				if len(openTools) > 0 {
					return &acceptedStreamError{msg: fmt.Sprintf("non-tool content block start at position %d while a tool block is open", i)}
				}
				nonTool = &openNonTool{index: idx, kind: textBlockOpen}
			case *pbv2.ContentBlockStart_Thinking:
				if nonTool != nil {
					return &acceptedStreamError{msg: fmt.Sprintf("non-tool content block start at position %d while a %s block is open", i, nonTool.kind)}
				}
				if len(openTools) > 0 {
					return &acceptedStreamError{msg: fmt.Sprintf("non-tool content block start at position %d while a tool block is open", i)}
				}
				nonTool = &openNonTool{index: idx, kind: thinkingBlockOpen}
			case *pbv2.ContentBlockStart_Provider:
				if nonTool != nil {
					return &acceptedStreamError{msg: fmt.Sprintf("non-tool content block start at position %d while a %s block is open", i, nonTool.kind)}
				}
				if len(openTools) > 0 {
					return &acceptedStreamError{msg: fmt.Sprintf("non-tool content block start at position %d while a tool block is open", i)}
				}
				nonTool = &openNonTool{index: idx, kind: providerBlockOpen}
			case *pbv2.ContentBlockStart_ToolCall:
				if nonTool != nil {
					return &acceptedStreamError{msg: fmt.Sprintf("tool call block start at position %d while a non-tool block is open", i)}
				}
				openTools[idx] = true
			}
		case *pbv2.StreamEvent_ContentBlockStop:
			idx := e.ContentBlockStop.Index
			if openTools[idx] {
				delete(openTools, idx)
				continue
			}
			if nonTool != nil && nonTool.index == idx {
				nonTool = nil
				spanOpen = false
				continue
			}
			return &acceptedStreamError{msg: fmt.Sprintf("content block stop at position %d names no open block", i)}
		case *pbv2.StreamEvent_TextDelta:
			if nonTool != nil && nonTool.kind != textBlockOpen {
				return &acceptedStreamError{msg: fmt.Sprintf("text delta at position %d inside a %s block", i, nonTool.kind)}
			}
			if nonTool == nil && len(openTools) > 0 {
				return &acceptedStreamError{msg: fmt.Sprintf("text delta at position %d while a tool block is open", i)}
			}
			spanOpen, sawText = true, true
		case *pbv2.StreamEvent_ThinkingDelta:
			if nonTool != nil && nonTool.kind != thinkingBlockOpen {
				return &acceptedStreamError{msg: fmt.Sprintf("thinking delta at position %d inside a %s block", i, nonTool.kind)}
			}
			if nonTool == nil && len(openTools) > 0 {
				return &acceptedStreamError{msg: fmt.Sprintf("thinking delta at position %d while a tool block is open", i)}
			}
			spanOpen, sawText = true, true
		case *pbv2.StreamEvent_SignatureDelta:
			// Mirrors the scope walk: an open span or an open text/thinking
			// block is current (the signature ends the span); a signature
			// inside an open tool/provider block, or one with no text/thinking
			// content anywhere, binds nothing.
			if spanOpen || (nonTool != nil && (nonTool.kind == textBlockOpen || nonTool.kind == thinkingBlockOpen)) {
				spanOpen = false
				continue
			}
			if (nonTool != nil && nonTool.kind == providerBlockOpen) || len(openTools) > 0 || !sawText {
				return &acceptedStreamError{msg: "signature_delta has no covered content (does not bind tool-call blocks)"}
			}
		case *pbv2.StreamEvent_ToolCallDelta:
			if !openTools[e.ToolCallDelta.Index] {
				return &acceptedStreamError{msg: fmt.Sprintf("tool call delta at position %d names no open tool block (%d)", i, e.ToolCallDelta.Index)}
			}
		}
	}

	if !sawError && (nonTool != nil || len(openTools) > 0) {
		return &acceptedStreamError{msg: "stream ends with an open content block; missing ContentBlockStop (or StreamError)"}
	}
	return nil
}

// --- typed text/thinking content -------------------------------------------

// typedContent is the content of a text/thinking span as a TYPED record:
// separate text and thinking accumulators, so identical bytes in different
// kinds differ (round-1 finding: a scope is a typed record — a signature over
// text "A" must not match a signature over thinking "A").
//
// (Round-4 F1) textSeen/thinkingSeen carry arm PRESENCE independently of
// bytes: an explicit block of the kind, or a delta of the kind — including an
// EMPTY delta — marks the arm even when it holds no bytes. Without presence,
// an empty TEXT scope and an empty THINKING scope collapsed into the same
// zero record {text:"", thinking:""}, so a token carried from an empty text
// span to an empty thinking span was consumed as an EXACT (token,
// typed-content) match and passed with every grant. Exact matching now
// requires the same presence, so the empty kinds can never match each other:
// the kind swap is classified stale, exactly like the non-empty case.
type typedContent struct {
	text         string
	thinking     string
	textSeen     bool // a text delta (even empty) or an explicit text block marked the arm
	thinkingSeen bool // a thinking delta (even empty) or an explicit thinking block marked the arm
}

func (t typedContent) equals(o typedContent) bool {
	return t.text == o.text && t.thinking == o.thinking &&
		t.textSeen == o.textSeen && t.thinkingSeen == o.thinkingSeen
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
	// (the signature ends the span it covers), and at end of stream. An
	// explicit text/thinking block closed with zero deltas contributes an
	// EMPTY span (round-2 F3): the block is present even without content,
	// so a missing span still means the block itself was suppressed. A
	// signature in an explicit text/thinking block with no deltas yet
	// contributes its empty span AT THE SIGNATURE EVENT (round-3 F1), so
	// later deltas become the next ordinal.
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
//	  the span closes (the signature ends the provider part it came with). A
//	  signature in an explicit text/thinking block with NO deltas yet
//	  materializes its EMPTY span at the signature event (round-3 F1). If
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
	var blockSawSpan bool // the open explicit text/thinking block already closed a span (round-2 F3)
	var spanOpen bool
	var spanText, spanThink strings.Builder
	var spanTextSeen, spanThinkSeen bool // arm presence of the OPEN span (round-4 F1)
	var closedText, closedThink strings.Builder
	var closedTextSeen, closedThinkSeen bool // OR'd arm presence over every span closed so far
	var sawText bool
	openTools := make(map[int32]*toolScopeBuilder)

	closeSpan := func() {
		if !spanOpen {
			return
		}
		tc := typedContent{text: spanText.String(), thinking: spanThink.String(), textSeen: spanTextSeen, thinkingSeen: spanThinkSeen}
		closedText.WriteString(tc.text)
		closedThink.WriteString(tc.thinking)
		closedTextSeen = closedTextSeen || tc.textSeen
		closedThinkSeen = closedThinkSeen || tc.thinkingSeen
		v.spans = append(v.spans, tc)
		spanText.Reset()
		spanThink.Reset()
		spanTextSeen, spanThinkSeen = false, false
		spanOpen = false
		if blockOpen && blockText {
			blockSawSpan = true
		}
	}

	for _, ev := range events {
		switch e := ev.Event.(type) {
		case *pbv2.StreamEvent_ContentBlockStart:
			closeSpan()
			cbs := e.ContentBlockStart
			switch cbs.Block.(type) {
			case *pbv2.ContentBlockStart_Text:
				blockOpen, blockText = true, true
				blockSawSpan = false
				spanTextSeen = true // the explicit block's kind marks the empty scope (round-4 F1)
			case *pbv2.ContentBlockStart_Thinking:
				blockOpen, blockText = true, true
				blockSawSpan = false
				spanThinkSeen = true
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
			if blockOpen && blockText && !blockSawSpan && !spanOpen {
				// An explicit text/thinking block closed with zero deltas is
				// still a span with EMPTY typed content (round-2 F3): the
				// block exists even without content, so an empty signed block
				// whose token disappears is a dropped signature — not a
				// suppressed block, which is what absence from the span list
				// would otherwise claim. The span carries the BLOCK KIND's
				// presence (round-4 F1), so an empty text block's span can
				// never be mistaken for an empty thinking block's.
				v.spans = append(v.spans, typedContent{textSeen: spanTextSeen, thinkingSeen: spanThinkSeen})
			}
			closeSpan()
			blockOpen, blockText, blockSawSpan = false, false, false
		case *pbv2.StreamEvent_ToolCallDelta:
			if b, ok := openTools[e.ToolCallDelta.Index]; ok {
				b.args.WriteString(e.ToolCallDelta.ArgumentsDelta)
			}
		case *pbv2.StreamEvent_TextDelta:
			if (blockOpen && !blockText) || len(openTools) > 0 {
				continue // ABI-violating text; charged by the strict walk
			}
			sawText = true
			spanTextSeen = true // even an EMPTY delta marks the arm (round-4 F1)
			spanText.WriteString(e.TextDelta)
			spanOpen = true
		case *pbv2.StreamEvent_ThinkingDelta:
			if (blockOpen && !blockText) || len(openTools) > 0 {
				continue // ABI-violating thinking; charged by the strict walk
			}
			sawText = true
			spanThinkSeen = true // even an EMPTY delta marks the arm (round-4 F1)
			spanThink.WriteString(e.ThinkingDelta)
			spanOpen = true
		case *pbv2.StreamEvent_SignatureDelta:
			switch {
			case spanOpen || (blockOpen && blockText):
				// Open text/thinking span: current-block binding over the
				// span's typed deltas up to this signature. A signature in an
				// explicit text/thinking block with NO deltas yet covers an
				// EMPTY span (round-3 F1): materialize that span AT THE
				// SIGNATURE EVENT, mark the block as having closed a span, and
				// leave later deltas to become the next ordinal — otherwise
				// the empty signed scope would vanish into a later span's
				// "rewritten" verdict when its token dropped. The materialized
				// span and the binding carry the BLOCK KIND's presence
				// (round-4 F1), so an empty text scope and an empty thinking
				// scope are distinct records.
				span := len(v.spans)
				if !spanOpen && blockOpen && blockText {
					v.spans = append(v.spans, typedContent{textSeen: spanTextSeen, thinkingSeen: spanThinkSeen})
					blockSawSpan = true
				}
				v.bindings = append(v.bindings, sigBinding{
					kind:      sigBindingCurrent,
					signature: e.SignatureDelta,
					content:   typedContent{text: spanText.String(), thinking: spanThink.String(), textSeen: spanTextSeen, thinkingSeen: spanThinkSeen},
					span:      span,
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
					content:   typedContent{text: closedText.String(), thinking: closedThink.String(), textSeen: closedTextSeen, thinkingSeen: closedThinkSeen},
					span:      -1,
				})
			}
		}
	}
	closeSpan() // an implicit span still in flight at end of stream
	v.closed = typedContent{text: closedText.String(), thinking: closedThink.String(), textSeen: closedTextSeen, thinkingSeen: closedThinkSeen}
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
// it. This is a TRUE GLOBAL pass over ALL returned scopes with a consumed
// set (round-3 F2): every exact full-fact occurrence is consumed before any
// correlation runs, so a modified block's same-index correlation can never
// steal an accepted occurrence that a later exact match owns. Pass-through
// AND a coherent block reindex (all facts moving together across indexes)
// both land here: a reindex is NOT forged — the index movement is a
// topology change whose charge belongs to 2b's full walk.
//
// Phase 2 — a returned scope that is not an exact repeat is correlated with
// the accepted scope at the SAME index (if one is unconsumed) via
// ClassifySignatureMutation, which decides cleared/dropped/stale/forged/added
// from the token change and the scope diff. A token that did NOT move with
// its block's facts (token-detached reindex) is caught here as stale.
//
// Phase 2b — (round-2 F1) cross-index correlation by IDENTICAL covered facts.
// A returned scope whose (id, name, assembled arguments) — not the index, not
// the token — reproduce an unconsumed accepted scope one-to-one at ANY index
// is the SAME block reindexed; ClassifySignatureMutation decides the token
// over the unchanged content. The same-index form of a dropped signature is
// rejected, and changing only the index must not bypass that: reindexing is
// topology, but it must not erase signature/content correlation. With the
// facts identical the only possible verdicts are dropped (token stripped) and
// forged (token replaced) — both rejected regardless of grants.
//
// Phase 3 — a returned scope with no accepted counterpart is invented: a
// SIGNED invented block is a minted signature (added) and is rejected
// regardless of grants (round-1 decision 2: the signature verifier is the
// single implementation of bound-signature semantics); an UNSIGNED invented
// block is a cardinality change and needs ir.stream.write.
//
// Phase 4 — accepted scopes no returned scope consumed are suppressed: a
// SIGNED block's disappearance is the recorded host obligation and is
// topology-gated; an UNSIGNED block's suppression is pure topology, the full
// field-policy walk's business, and passes here by design.
func verifyToolScopes(accepted, returned []toolCallScope, canWrite func(string) bool) error {
	consumed := make([]bool, len(accepted))
	retConsumed := make([]bool, len(returned))

	// Phase 1 — a TRUE GLOBAL exact-first pass (round-3 F2): every returned
	// scope that reproduces an accepted scope's complete facts (signature,
	// id, name, arguments) at ANY index consumes it, and NO correlation runs
	// until every exact full-fact occurrence is consumed. Scanning exact
	// matches inside the per-returned loop let a MODIFIED block's same-index
	// correlation consume an accepted occurrence first, forcing a later
	// exact match into a forged verdict — output-order-dependent. The
	// consumed set makes the fallback phases see the exact matches as taken,
	// exactly as verifySignatureDeltaBindings already does.
	for j := range returned {
		for i := range accepted {
			if !consumed[i] &&
				accepted[i].signature == returned[j].signature &&
				accepted[i].id == returned[j].id &&
				accepted[i].name == returned[j].name &&
				accepted[i].arguments == returned[j].arguments {
				consumed[i] = true
				retConsumed[j] = true
				break
			}
		}
	}

	for j, r := range returned {
		if retConsumed[j] {
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

		// Phase 2b: cross-index correlation by identical covered facts.
		matched := -1
		for i := range accepted {
			if !consumed[i] &&
				accepted[i].id == r.id &&
				accepted[i].name == r.name &&
				accepted[i].arguments == r.arguments {
				matched = i
				break
			}
		}
		if matched >= 0 {
			class := outboundpolicy.ClassifySignatureMutation(
				accepted[matched].signature, r.signature, false)
			if !class.Allowed() {
				return fmt.Errorf("tool block %d signature %s", r.index, class)
			}
			consumed[matched] = true
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
// Phase 3 — accepted occurrences no returned token covers. An explicit empty
// returned occurrence is a clear marker belonging to the returned SPAN it was
// emitted in — never a pool of interchangeable markers to be consumed
// positionally (round-2 F2): an unchanged accepted signed occurrence surviving
// anywhere without its token is rejected as dropped BEFORE any clear marker
// is assigned, so a marker attached to a DIFFERENT (rewritten) span cannot
// hide a dropped token. The unchanged-content precheck is OCCURRENCE-AWARE
// (round-3 F3): returned spans are consumed one-to-one, matching accepted
// UNSIGNED twins before they can prove a signed occurrence dropped, so a
// surplus occurrence condemns (dropped) while a twin-only survival is a
// suppressed signed span (topology-gated). The returned spans owned by the
// EXACT matches Phase 1 consumed are reserved from that multiset first
// (round-4 F2): a surviving exact signed occurrence is its match's own span,
// never surplus evidence against a second identical signed occurrence. After
// that gate, a marker whose typed content exactly matches an accepted
// occurrence is that occurrence's token stripped over unchanged content
// (dropped, rejected); a marker otherwise clears the accepted signed
// occurrence at ITS OWN span ordinal (that span's content was rewritten).
// What remains is decided from the returned span structure by
// verifyUnpairedBinding: a missing span at the binding's position means the
// signed block itself was suppressed (topology-gated), an EMPTY signed scope
// with no surviving empty span is a dropped span (round-3 F1), and different
// content at the position is clearing (allowed).
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
		var emptyRet []sigBinding // explicit empty returned occurrences, stream order
		for j := range ret {
			if ret[j].signature == "" {
				emptyRet = append(emptyRet, ret[j])
			}
		}
		emptyUsed := make([]bool, len(emptyRet))
		exactEmpty := func(c typedContent) int {
			for k, m := range emptyRet {
				if !emptyUsed[k] && m.content.equals(c) {
					return k
				}
			}
			return -1
		}
		// (a) Occurrence-aware multiset precheck (round-3 F3): the old check
		// asked whether the accepted signed content appears ANYWHERE in the
		// returned spans, ignoring multiplicity and unsigned twins — a
		// surviving identical UNSIGNED twin was misread as proof the signed
		// token was stripped, so an allowed signed suppression looked like a
		// dropped token. Consumption is now one-to-one, the same cardinality
		// discipline the request verifier applies to section fingerprints:
		//
		//   (b) surviving unchanged returned spans are matched against
		//       accepted UNSIGNED twins FIRST (an accepted span that no
		//       accepted signed binding covers), so a twin can never prove a
		//       signed occurrence dropped;
		//   (c) any SURPLUS unchanged returned occurrence after that is the
		//       signed content surviving without its token → dropped;
		//   (d) an accepted signed occurrence whose content survived only as
		//       a consumed twin was SUPPRESSED while the twin lived on →
		//       topology-gated on ir.stream.write.
		var remaining map[typedContent]int
		var twinSurvived map[typedContent]bool
		if kind == sigBindingCurrent {
			remaining = make(map[typedContent]int, len(rv.spans))
			for _, s := range rv.spans {
				remaining[s]++
			}
			// (round-4 F2) Reserve the returned span ordinals owned by the
			// exact matches Phase 1 consumed: the multiset was rebuilt from
			// ALL returned spans, so a surviving exact signed occurrence was
			// left available to prove a SECOND identical signed occurrence
			// dropped. Subtract every exact-consumed returned binding's own
			// span BEFORE unsigned twins (or anything else) may claim it;
			// only a SURPLUS returned occurrence can then condemn a signed
			// binding, and an exact match's own span is never surplus.
			for j := range ret {
				if retConsumed[j] && ret[j].span >= 0 && ret[j].span < len(rv.spans) {
					remaining[rv.spans[ret[j].span]]--
				}
			}
			signedOrdinals := make(map[int]bool, len(acc))
			for _, b := range acc {
				if b.signature != "" && b.span >= 0 {
					signedOrdinals[b.span] = true
				}
			}
			twinSurvived = make(map[typedContent]bool)
			for o, s := range av.spans {
				if signedOrdinals[o] || remaining[s] == 0 {
					continue
				}
				remaining[s]--
				twinSurvived[s] = true
			}
		}

		for i := range acc {
			if consumed[i] || acc[i].signature == "" {
				continue
			}
			if kind == sigBindingTrailing {
				if rv.closed.equals(acc[i].content) {
					return fmt.Errorf("%s signature_delta dropped", kind)
				}
			} else if remaining[acc[i].content] > 0 {
				return fmt.Errorf("%s signature_delta dropped", kind)
			}
			// Exact empty-marker match: the plugin emitted an explicit empty
			// signature_delta beside the SAME typed content — the token was
			// stripped from content the provider actually signed.
			if k := exactEmpty(acc[i].content); k >= 0 {
				emptyUsed[k] = true
				consumed[i] = true
				return fmt.Errorf("%s signature_delta dropped", kind)
			}
			// Clear markers correlate by their OWN returned span identity
			// (round-2 F2): the marker emitted in returned span j is the
			// prescribed clearing response for the accepted signed occurrence
			// at span j — that span's content was rewritten, so (a) did not
			// reject it. A marker is never consumed for a DIFFERENT accepted
			// occurrence merely because it is the next unused one.
			cleared := -1
			for k, m := range emptyRet {
				if !emptyUsed[k] && m.span == acc[i].span {
					cleared = k
					break
				}
			}
			if cleared >= 0 {
				emptyUsed[cleared] = true
				consumed[i] = true
				continue
			}
			if kind == sigBindingCurrent && twinSurvived[acc[i].content] {
				// (d) The signed content survived only as an unsigned twin:
				// the signed span itself was suppressed (topology), and the
				// twin is what remains.
				if !canWrite(streamWriteGrant) {
					return fmt.Errorf("suppressed a signed text/thinking block without %s", streamWriteGrant)
				}
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
// structure alone (the unchanged-content survival cases were already decided
// by the occurrence-aware precheck in verifySignatureDeltaBindings):
//
//   - nothing occupies the position (current-block) or the turn has no text
//     left at all (trailing) → the signed block itself was suppressed →
//     topology-gated on ir.stream.write;
//   - the signed scope is EMPTY (round-3 F1) → its empty span was dropped:
//     there is no content to rewrite, so "cleared" cannot apply;
//   - different content occupies the position → cleared (the prescribed
//     response to a legitimate rewrite) → allowed.
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
	// CurrentContentBlock: the signed content surviving as a surplus
	// occurrence was already condemned by the occurrence-aware precheck
	// ((c)/(d) in verifySignatureDeltaBindings), so here only the span
	// STRUCTURE decides: nothing at the binding's ordinal is a suppressed
	// block, an EMPTY signed scope cannot be rewritten (round-3 F1) and is a
	// dropped span, and anything else is a cleared token over rewritten
	// content.
	if ab.span >= 0 && ab.span >= len(rv.spans) {
		if !canWrite(streamWriteGrant) {
			return fmt.Errorf("suppressed a signed text/thinking block without %s", streamWriteGrant)
		}
		return nil
	}
	if ab.content.isEmpty() {
		// round-3 F1: a signature over ZERO deltas is either intact
		// (consumed by an exact match in phase 1) or its empty span was
		// dropped — there is no content to rewrite, so "cleared" cannot
		// apply, and the block still exists (a span sits at the ordinal), so
		// this is not suppression either.
		return fmt.Errorf("current-block signature_delta dropped")
	}
	return nil // a different span sits at the position: cleared
}
