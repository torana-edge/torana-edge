package plugin

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/torana-edge/torana-plugin-sdk/outboundpolicy"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// Tests for the stream signature verifier (Migration B part 2a, reworked per
// the #243 round-1 findings).
//
// The scope definitions — typed CurrentContentBlock and TrailingStandalone
// over the single-pass event walk in scanStreamSignatures — are pinned here
// directly (TestScanStreamSignaturesPinsTheScopeWalk), and the accepted-side
// discipline split is pinned by TestValidateAcceptedStream and
// TestStreamVerifyAcceptedSideSeparation.
//
// The SDK's SignatureStreamFixtures are the cross-repo transactional contract:
// a verifier is correct on the ToolCallBlockByIndex axis exactly when, given
// Accepted and Returned, its OWN scope diff plus ClassifySignatureMutation
// yields Want for the block at Index. TestVerifyStreamConformsToSDKStreamFixtures
// runs verifyStream over every fixture (including BOTH concurrent-tool twins),
// and TestStreamVerifyWrongScopesFailTheFixtures proves the scope functions
// are not one of the three wrong implementations the fixtures exist to catch.

const streamSigA = "provider-token-a"
const streamSigB = "provider-token-b"

func textDelta(s string) *pbv2.StreamEvent {
	return &pbv2.StreamEvent{Event: &pbv2.StreamEvent_TextDelta{TextDelta: s}}
}

func thinkingDelta(s string) *pbv2.StreamEvent {
	return &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ThinkingDelta{ThinkingDelta: s}}
}

func signatureDelta(sig string) *pbv2.StreamEvent {
	return &pbv2.StreamEvent{Event: &pbv2.StreamEvent_SignatureDelta{SignatureDelta: sig}}
}

func streamErrorEvent() *pbv2.StreamEvent {
	return &pbv2.StreamEvent{Event: &pbv2.StreamEvent_Error{
		Error: &pbv2.StreamError{Code: 13, Message: "upstream failure"},
	}}
}

func toolStartEvent(index int32, id, name string) *pbv2.StreamEvent {
	return &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStart{
		ContentBlockStart: &pbv2.ContentBlockStart{
			Index: index,
			Block: &pbv2.ContentBlockStart_ToolCall{ToolCall: &pbv2.ToolCallRef{Id: id, Name: name}},
		},
	}}
}

func toolBlock(index int32, id, name, signature, args string) []*pbv2.StreamEvent {
	return toolBlockDeltas(index, id, name, signature, args)
}

// toolBlockDeltas renders one signed tool block whose arguments arrive as the
// given fragments, so tests can vary framing independently of content — same
// helper the SDK fixtures use.
func toolBlockDeltas(index int32, id, name, signature string, args ...string) []*pbv2.StreamEvent {
	out := []*pbv2.StreamEvent{{Event: &pbv2.StreamEvent_ContentBlockStart{
		ContentBlockStart: &pbv2.ContentBlockStart{
			Index: index,
			Block: &pbv2.ContentBlockStart_ToolCall{ToolCall: &pbv2.ToolCallRef{
				Id: id, Name: name, Signature: signature,
			}},
		},
	}}}
	for _, a := range args {
		out = append(out, &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ToolCallDelta{
			ToolCallDelta: &pbv2.ToolCallDelta{Index: index, ArgumentsDelta: a},
		}})
	}
	return append(out, &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStop{
		ContentBlockStop: &pbv2.ContentBlockStop{Index: index},
	}})
}

// textBlock renders an explicit text block with ContentBlockStart/Stop
// boundaries — the ABI-conformant representation of text/thinking content.
func textBlock(index int32, texts ...string) []*pbv2.StreamEvent {
	out := []*pbv2.StreamEvent{{Event: &pbv2.StreamEvent_ContentBlockStart{
		ContentBlockStart: &pbv2.ContentBlockStart{
			Index: index,
			Block: &pbv2.ContentBlockStart_Text{Text: &pbv2.TextBlock{}},
		},
	}}}
	for _, t := range texts {
		out = append(out, textDelta(t))
	}
	return append(out, &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStop{
		ContentBlockStop: &pbv2.ContentBlockStop{Index: index},
	}})
}

// thinkingBlock renders an explicit thinking block.
func thinkingBlock(index int32, texts ...string) []*pbv2.StreamEvent {
	out := []*pbv2.StreamEvent{{Event: &pbv2.StreamEvent_ContentBlockStart{
		ContentBlockStart: &pbv2.ContentBlockStart{
			Index: index,
			Block: &pbv2.ContentBlockStart_Thinking{Thinking: &pbv2.ThinkingBlock{}},
		},
	}}}
	for _, t := range texts {
		out = append(out, thinkingDelta(t))
	}
	return append(out, &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStop{
		ContentBlockStop: &pbv2.ContentBlockStop{Index: index},
	}})
}

func noGrants(string) bool { return false }

func withStreamWrite(string) bool { return true }

// withEveryGrant reports every grant held — used to prove that invented
// SIGNED blocks are rejected even when no grant could save them.
func withEveryGrant(string) bool { return true }

// isAcceptedErr reports whether err is an *acceptedStreamError (a host/adaptor
// defect in the accepted stream, never a plugin failure).
func isAcceptedErr(err error) bool {
	var ae *acceptedStreamError
	return errors.As(err, &ae)
}

// signedTextBlock renders an explicit text block with a signature_delta
// INSIDE it — a current-block binding over the typed text (the signature
// closes the block's span, matching the provider-part model). sig may be ""
// to render an explicit empty clear marker.
func signedTextBlock(index int32, text, sig string) []*pbv2.StreamEvent {
	out := []*pbv2.StreamEvent{{Event: &pbv2.StreamEvent_ContentBlockStart{
		ContentBlockStart: &pbv2.ContentBlockStart{
			Index: index, Block: &pbv2.ContentBlockStart_Text{Text: &pbv2.TextBlock{}},
		},
	}}}
	out = append(out, textDelta(text))
	out = append(out, signatureDelta(sig))
	return append(out, &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStop{
		ContentBlockStop: &pbv2.ContentBlockStop{Index: index},
	}})
}

func messageStopEvent(finishReason string) *pbv2.StreamEvent {
	return &pbv2.StreamEvent{Event: &pbv2.StreamEvent_MessageStop{
		MessageStop: &pbv2.MessageStop{FinishReason: finishReason},
	}}
}

func usageEvent() *pbv2.StreamEvent {
	return &pbv2.StreamEvent{Event: &pbv2.StreamEvent_Usage{
		Usage: &pbv2.Usage{InputTokens: 1, OutputTokens: 1},
	}}
}

// TestVerifyStreamConformsToSDKStreamFixtures runs the SDK's cross-repo
// transactional contract through verifyStream's signature axis: for every
// fixture, an Allowed Want means verifyStream must return nil, and a rejected
// Want means it must return an error naming the class. This includes BOTH
// concurrent-tool twins (index 0 and index 1 of the interleaved shape), which
// the round-1 verdict pinned: a verifier that tracks a single open tool block
// fails one of them.
func TestVerifyStreamConformsToSDKStreamFixtures(t *testing.T) {
	fx := outboundpolicy.SignatureStreamFixtures()
	if len(fx) == 0 {
		t.Fatal("no fixtures, so this asserts nothing")
	}
	seen := map[outboundpolicy.SignatureMutation]bool{}
	for _, f := range fx {
		t.Run(f.Name, func(t *testing.T) {
			err := verifyStream(f.Accepted, f.Returned, noGrants)
			if f.Want.Allowed() {
				if err != nil {
					t.Fatalf("allowed fixture rejected: %v\n%s", err, f.Why)
				}
				return
			}
			if err == nil {
				t.Fatalf("fixture with Want %v passed\n%s", f.Want, f.Why)
			}
			if !strings.Contains(err.Error(), f.Want.String()) {
				t.Fatalf("error does not name the class %v: %v\n%s", f.Want, err, f.Why)
			}
		})
		seen[f.Want] = true
	}
	for _, m := range []outboundpolicy.SignatureMutation{
		outboundpolicy.SignatureIntact, outboundpolicy.SignatureCleared,
		outboundpolicy.SignatureDropped, outboundpolicy.SignatureStale,
		outboundpolicy.SignatureForged, outboundpolicy.SignatureAdded,
	} {
		if !seen[m] {
			t.Errorf("no fixture covers %v", m)
		}
	}

	// The concurrent-tool shape must classify BOTH indexes (the SDK pins this
	// too): a fixture asking only about index 0 lets a verifier record the
	// first open tool while ignoring the second concurrently opened one.
	var concurrent int
	for _, f := range fx {
		var sawStarts []int32
		for _, ev := range f.Accepted {
			if s, ok := ev.Event.(*pbv2.StreamEvent_ContentBlockStart); ok && s.ContentBlockStart.GetToolCall() != nil {
				sawStarts = append(sawStarts, s.ContentBlockStart.Index)
			}
		}
		if len(sawStarts) >= 2 {
			concurrent++
		}
	}
	if concurrent < 2 {
		t.Fatalf("expected the concurrent shape to carry at least two fixtures (one per block), got %d", concurrent)
	}
}

// TestStreamVerifyWrongScopesFailTheFixtures mirrors the SDK's
// TestWrongVerifiersFailTheFixtures against OUR scope computation: the three
// mistakes the fixtures exist to catch must each fail at least one fixture,
// while the real scope functions classify every fixture as Want.
func TestStreamVerifyWrongScopesFailTheFixtures(t *testing.T) {
	// Pools every delta regardless of block index.
	poolsIndexes := func(events []*pbv2.StreamEvent, index int32) toolCallScope {
		s := toolCallScopeOf(events, index)
		s.arguments = ""
		for _, ev := range events {
			if d, ok := ev.Event.(*pbv2.StreamEvent_ToolCallDelta); ok {
				s.arguments += d.ToolCallDelta.ArgumentsDelta
			}
		}
		return s
	}
	// Signs only the arguments, omitting id and name from the scope.
	argsOnly := func(a, b toolCallScope) bool { return a.arguments != b.arguments }
	// Compares the first fragment instead of the assembled arguments.
	firstFragment := func(events []*pbv2.StreamEvent, index int32) toolCallScope {
		s := toolCallScopeOf(events, index)
		s.arguments = ""
		for _, ev := range events {
			if d, ok := ev.Event.(*pbv2.StreamEvent_ToolCallDelta); ok && d.ToolCallDelta.Index == index {
				s.arguments = d.ToolCallDelta.ArgumentsDelta
				break
			}
		}
		return s
	}

	for _, w := range []struct {
		name    string
		scope   func([]*pbv2.StreamEvent, int32) toolCallScope
		changed func(a, b toolCallScope) bool
	}{
		{"pools deltas across block indexes", poolsIndexes, toolCallContentChanged},
		{"omits id and name from the signed scope", toolCallScopeOf, argsOnly},
		{"compares only the first fragment", firstFragment, toolCallContentChanged},
	} {
		t.Run(w.name, func(t *testing.T) {
			for _, f := range outboundpolicy.SignatureStreamFixtures() {
				before, after := w.scope(f.Accepted, f.Index), w.scope(f.Returned, f.Index)
				got := outboundpolicy.ClassifySignatureMutation(
					before.signature, after.signature, w.changed(before, after))
				if got != f.Want {
					return // caught, as it must be
				}
			}
			t.Fatalf("a verifier that %s reproduced every fixture; "+
				"the suite does not discriminate on this axis", w.name)
		})
	}

	// The real scope functions must classify every fixture as declared.
	for _, f := range outboundpolicy.SignatureStreamFixtures() {
		before, after := toolCallScopeOf(f.Accepted, f.Index), toolCallScopeOf(f.Returned, f.Index)
		got := outboundpolicy.ClassifySignatureMutation(
			before.signature, after.signature, toolCallContentChanged(before, after))
		if got != f.Want {
			t.Fatalf("fixture %q: real scope classified %v, want %v\n%s", f.Name, got, f.Want, f.Why)
		}
	}
}

// TestScanStreamSignaturesPinsTheScopeWalk pins the exact event-walk
// semantics: which signature_delta lands in which scope, with which TYPED
// covered content, in both the boundary-less host representation and the
// ABI-conformant explicit-block representation. The typed record is the
// round-1 finding: identical bytes in the text and thinking kinds must differ.
func TestScanStreamSignaturesPinsTheScopeWalk(t *testing.T) {
	// Boundary-less host representation: a bare run of text deltas closed by
	// a signature is one current binding over the typed text.
	v := scanStreamSignatures([]*pbv2.StreamEvent{textDelta("t1"), textDelta("t2"), signatureDelta(streamSigA)})
	if len(v.bindings) != 1 || v.bindings[0].kind != sigBindingCurrent {
		t.Fatalf("boundary-less run: bindings = %+v, want one current binding", v.bindings)
	}
	if got := v.bindings[0].content; got.text != "t1t2" || got.thinking != "" {
		t.Fatalf("boundary-less run: content = %+v, want typed text t1t2", got)
	}

	// Thinking accumulates in its own slot: same bytes, different kind.
	v = scanStreamSignatures([]*pbv2.StreamEvent{thinkingDelta("A"), signatureDelta(streamSigA)})
	if len(v.bindings) != 1 || v.bindings[0].content.text != "" || v.bindings[0].content.thinking != "A" {
		t.Fatalf("thinking run: bindings = %+v, want typed thinking A", v.bindings)
	}
	v = scanStreamSignatures([]*pbv2.StreamEvent{textDelta("A"), signatureDelta(streamSigA)})
	if len(v.bindings) != 1 || v.bindings[0].content.text != "A" || v.bindings[0].content.thinking != "" {
		t.Fatalf("text run: bindings = %+v, want typed text A", v.bindings)
	}

	// A span can carry BOTH kinds (boundary-less host IR interleaves them);
	// the record keeps the slots apart.
	v = scanStreamSignatures([]*pbv2.StreamEvent{textDelta("x"), thinkingDelta("y"), signatureDelta(streamSigA)})
	if len(v.bindings) != 1 {
		t.Fatalf("mixed span: got %d bindings, want 1", len(v.bindings))
	}
	if got := v.bindings[0].content; got.text != "x" || got.thinking != "y" {
		t.Fatalf("mixed span: content = %+v, want typed (x, y)", got)
	}

	// A signature ends the span it covers; a later part's text opens a fresh
	// span (the provider part model).
	v = scanStreamSignatures([]*pbv2.StreamEvent{
		textDelta("p1"), signatureDelta(streamSigA), textDelta("p2"), signatureDelta(streamSigB),
	})
	if len(v.bindings) != 2 {
		t.Fatalf("two parts: got %d bindings, want 2", len(v.bindings))
	}
	if v.bindings[0].kind != sigBindingCurrent || v.bindings[0].content.text != "p1" || v.bindings[0].span != 0 {
		t.Fatalf("first binding = %+v, want current over p1 at span 0", v.bindings[0])
	}
	if v.bindings[1].kind != sigBindingCurrent || v.bindings[1].content.text != "p2" || v.bindings[1].span != 1 {
		t.Fatalf("second binding = %+v, want current over p2 at span 1", v.bindings[1])
	}

	// ABI-conformant: signature inside the open block is current; signature
	// after the stop is trailing over the closed content.
	open := []*pbv2.StreamEvent{
		{Event: &pbv2.StreamEvent_ContentBlockStart{ContentBlockStart: &pbv2.ContentBlockStart{
			Index: 0, Block: &pbv2.ContentBlockStart_Text{Text: &pbv2.TextBlock{}},
		}}},
		textDelta("a"),
		signatureDelta(streamSigA),
		{Event: &pbv2.StreamEvent_ContentBlockStop{ContentBlockStop: &pbv2.ContentBlockStop{Index: 0}}},
	}
	v = scanStreamSignatures(open)
	if len(v.bindings) != 1 || v.bindings[0].kind != sigBindingCurrent || v.bindings[0].content.text != "a" {
		t.Fatalf("sig inside open text block: %+v, want current over a", v.bindings)
	}
	closed := append(textBlock(0, "a"), signatureDelta(streamSigA))
	v = scanStreamSignatures(closed)
	if len(v.bindings) != 1 || v.bindings[0].kind != sigBindingTrailing || v.bindings[0].content.text != "a" {
		t.Fatalf("sig after closed text block: %+v, want trailing over a", v.bindings)
	}

	// The concurrent-tool shape assembles one scope per index IN the walk
	// (no rescanning): the interleaved fixture's two blocks both appear with
	// their own arguments.
	var inter outboundpolicy.StreamFixture
	for _, f := range outboundpolicy.SignatureStreamFixtures() {
		if f.Index == 1 && f.Want == outboundpolicy.SignatureIntact {
			inter = f // the concurrent-tool twin asking about block 1
			break
		}
	}
	if inter.Accepted == nil {
		t.Fatal("no concurrent-tool fixture found")
	}
	v = scanStreamSignatures(inter.Accepted)
	if len(v.toolScopes) != 2 {
		t.Fatalf("interleaved shape: got %d tool scopes, want 2", len(v.toolScopes))
	}
	if v.toolScopes[0].index != 0 || v.toolScopes[0].arguments != `{"path":`+`"/a"}` ||
		v.toolScopes[1].index != 1 || v.toolScopes[1].arguments != `{"path":`+`"/b"}` {
		t.Fatalf("interleaved scopes not kept apart by index: %+v", v.toolScopes)
	}

	// No covered content at all — tool-call-only turn, leading signature,
	// signature inside a tool block — is unbound.
	v = scanStreamSignatures(append(toolBlock(0, "c1", "read_file", "", `{}`), signatureDelta(streamSigA)))
	if len(v.bindings) != 1 || v.bindings[0].kind != sigBindingUnbound {
		t.Fatalf("sig after tool-call-only content: %+v, want unbound", v.bindings)
	}
	v = scanStreamSignatures([]*pbv2.StreamEvent{signatureDelta(streamSigA)})
	if len(v.bindings) != 1 || v.bindings[0].kind != sigBindingUnbound {
		t.Fatalf("leading sig: %+v, want unbound", v.bindings)
	}
	v = scanStreamSignatures([]*pbv2.StreamEvent{toolStartEvent(0, "c1", "read_file"), signatureDelta(streamSigA)})
	if len(v.bindings) != 1 || v.bindings[0].kind != sigBindingUnbound {
		t.Fatalf("sig inside open tool block: %+v, want unbound", v.bindings)
	}
}

// TestStreamVerifyTypedScopes pins the round-1 typed-scope finding: a scope
// is a typed record, so identical bytes in different kinds MUST differ.
// Carrying a token from text "A" to thinking "A" (or vice versa) is stale.
func TestStreamVerifyTypedScopes(t *testing.T) {
	textSigned := []*pbv2.StreamEvent{textDelta("A"), signatureDelta(streamSigA)}
	thinkingSigned := []*pbv2.StreamEvent{thinkingDelta("A"), signatureDelta(streamSigA)}

	t.Run("identical text re-emission is intact", func(t *testing.T) {
		if err := verifyStream(textSigned, textSigned, noGrants); err != nil {
			t.Fatalf("identical text stream rejected: %v", err)
		}
	})
	t.Run("identical thinking re-emission is intact", func(t *testing.T) {
		if err := verifyStream(thinkingSigned, thinkingSigned, noGrants); err != nil {
			t.Fatalf("identical thinking stream rejected: %v", err)
		}
	})
	t.Run("text to thinking with the token kept is stale", func(t *testing.T) {
		err := verifyStream(textSigned, thinkingSigned, noGrants)
		if err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("text A -> thinking A with token kept: err = %v, want stale", err)
		}
	})
	t.Run("thinking to text with the token kept is stale", func(t *testing.T) {
		err := verifyStream(thinkingSigned, textSigned, noGrants)
		if err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("thinking A -> text A with token kept: err = %v, want stale", err)
		}
	})
	t.Run("token cleared across the kind boundary is allowed", func(t *testing.T) {
		returned := []*pbv2.StreamEvent{thinkingDelta("A")}
		if err := verifyStream(textSigned, returned, noGrants); err != nil {
			t.Fatalf("cleared token over rewritten kind rejected: %v", err)
		}
	})
	t.Run("kind swap within one span is stale", func(t *testing.T) {
		accepted := []*pbv2.StreamEvent{textDelta("x"), thinkingDelta("y"), signatureDelta(streamSigA)}
		returned := []*pbv2.StreamEvent{textDelta("y"), thinkingDelta("x"), signatureDelta(streamSigA)}
		err := verifyStream(accepted, returned, noGrants)
		if err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("swapped typed slots with token kept: err = %v, want stale", err)
		}
	})
	t.Run("explicit thinking block matches only thinking scope", func(t *testing.T) {
		explicit := append(thinkingBlock(0, "A"), signatureDelta(streamSigA))
		if err := verifyStream(explicit, explicit, noGrants); err != nil {
			t.Fatalf("identical explicit thinking stream rejected: %v", err)
		}
		err := verifyStream(explicit, append(textBlock(0, "A"), signatureDelta(streamSigA)), noGrants)
		if err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("explicit thinking A -> explicit text A with token kept: err = %v, want stale", err)
		}
	})
}

// TestStreamVerifyExactMatchFirst pins the round-1 correlation finding: exact
// (token, typed-content) matches are found and consumed BEFORE ambiguity or
// stale is diagnosed. Two signed spans sharing one token value pass through
// unchanged (the old verifier misread this as ambiguous), and the
// deletion/duplication/reuse/reorder cardinality is judged correctly.
func TestStreamVerifyExactMatchFirst(t *testing.T) {
	// The round-1 reproduction: the SAME token over two different contents.
	accepted := []*pbv2.StreamEvent{
		textDelta("first"), signatureDelta(streamSigA),
		textDelta("second"), signatureDelta(streamSigA),
	}
	t.Run("repeated token over different content passes through unchanged", func(t *testing.T) {
		if err := verifyStream(accepted, accepted, noGrants); err != nil {
			t.Fatalf("repeated token pass-through rejected: %v", err)
		}
	})
	t.Run("repeated token: mutating the second span is stale, not ambiguous", func(t *testing.T) {
		returned := []*pbv2.StreamEvent{
			textDelta("first"), signatureDelta(streamSigA),
			textDelta("second changed"), signatureDelta(streamSigA),
		}
		err := verifyStream(accepted, returned, noGrants)
		if err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("second span changed with token kept: err = %v, want stale", err)
		}
	})
	t.Run("repeated token: dropping the second span is a signed-block suppression", func(t *testing.T) {
		returned := []*pbv2.StreamEvent{textDelta("first"), signatureDelta(streamSigA)}
		if err := verifyStream(accepted, returned, noGrants); err == nil ||
			!strings.Contains(err.Error(), streamWriteGrant) {
			t.Fatalf("second span gone without grant: err = %v, want topology rejection", err)
		}
		if err := verifyStream(accepted, returned, withStreamWrite); err != nil {
			t.Fatalf("second span gone with grant rejected: %v", err)
		}
	})

	twoSigned := []*pbv2.StreamEvent{
		textDelta("first"), signatureDelta(streamSigA),
		textDelta("second"), signatureDelta(streamSigB),
	}
	t.Run("deletion: removing a signed span suppresses it", func(t *testing.T) {
		returned := []*pbv2.StreamEvent{textDelta("first"), signatureDelta(streamSigA)}
		if err := verifyStream(twoSigned, returned, noGrants); err == nil ||
			!strings.Contains(err.Error(), streamWriteGrant) {
			t.Fatalf("deleted signed span without grant: err = %v, want topology rejection", err)
		}
		if err := verifyStream(twoSigned, returned, withStreamWrite); err != nil {
			t.Fatalf("deleted signed span with grant rejected: %v", err)
		}
	})
	t.Run("duplication: an extra copy of a signed span is added", func(t *testing.T) {
		// Every accepted occurrence is consumed by an exact match, so the
		// extra copy has no counterpart: a token appeared where none was.
		accepted := []*pbv2.StreamEvent{textDelta("first"), signatureDelta(streamSigA)}
		returned := []*pbv2.StreamEvent{
			textDelta("first"), signatureDelta(streamSigA),
			textDelta("first"), signatureDelta(streamSigA),
		}
		err := verifyStream(accepted, returned, noGrants)
		if err == nil || !strings.Contains(err.Error(), "added") {
			t.Fatalf("duplicated signed span: err = %v, want added", err)
		}
	})
	t.Run("reuse: applying a spent token to new content is forged", func(t *testing.T) {
		returned := []*pbv2.StreamEvent{
			textDelta("first"), signatureDelta(streamSigA),
			textDelta("second"), signatureDelta(streamSigA),
		}
		err := verifyStream(twoSigned, returned, noGrants)
		if err == nil || !strings.Contains(err.Error(), "forged") {
			t.Fatalf("reused token over new content: err = %v, want forged", err)
		}
	})
	t.Run("reorder: spans moving order with exact facts intact", func(t *testing.T) {
		returned := []*pbv2.StreamEvent{
			textDelta("second"), signatureDelta(streamSigB),
			textDelta("first"), signatureDelta(streamSigA),
		}
		if err := verifyStream(twoSigned, returned, noGrants); err != nil {
			t.Fatalf("reordered spans rejected: %v", err)
		}
	})
}

// TestStreamVerifyReindex pins the round-1 reindex finding: tool blocks are
// assembled by index WITHIN each side and aligned one-to-one by their complete
// signed facts ACROSS sides. A coherent reindex — both blocks' (id, name,
// arguments, token) facts moving 0↔1 together — is NOT a signature violation
// at this layer; the index movement is a topology change whose charge belongs
// to 2b's full walk. A TOKEN-DETACHED reindex (facts move, tokens do not) is
// stale and IS caught.
func TestStreamVerifyReindex(t *testing.T) {
	accepted := append(
		toolBlock(0, "call_1", "read_file", streamSigA, `{"path":"/a"}`),
		toolBlock(1, "call_2", "write_file", streamSigB, `{"path":"/b"}`)...)
	reindexed := append(
		toolBlock(1, "call_2", "write_file", streamSigB, `{"path":"/b"}`),
		toolBlock(0, "call_1", "read_file", streamSigA, `{"path":"/a"}`)...)

	t.Run("coherent reindex is not forged", func(t *testing.T) {
		if err := verifyStream(accepted, reindexed, withStreamWrite); err != nil {
			t.Fatalf("coherent reindex rejected: %v", err)
		}
		// The signature layer does not charge the index movement at all — it
		// is 2b's topology walk that will. Documented boundary, pinned here.
		if err := verifyStream(accepted, reindexed, noGrants); err != nil {
			t.Fatalf("coherent reindex is a signature violation at this layer: %v", err)
		}
	})
	t.Run("token-detached reindex is stale", func(t *testing.T) {
		// The (id, name, arguments) facts moved 0↔1 but the tokens stayed
		// put: each token now sits over content the provider never signed.
		detached := append(
			toolBlock(0, "call_2", "write_file", streamSigA, `{"path":"/b"}`),
			toolBlock(1, "call_1", "read_file", streamSigB, `{"path":"/a"}`)...)
		err := verifyStream(accepted, detached, withEveryGrant)
		if err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("token-detached reindex: err = %v, want stale (even with every grant)", err)
		}
	})
}

// TestStreamVerifyInventedToolBlock pins round-1 decision 2: the signature
// verifier is the single implementation of bound-signature semantics, so an
// invented SIGNED tool block is a minted signature (added) and is rejected
// even with every grant; an invented UNSIGNED tool block changes cardinality
// and passes this layer with ir.stream.write.
func TestStreamVerifyInventedToolBlock(t *testing.T) {
	accepted := []*pbv2.StreamEvent{textDelta("ok")}

	t.Run("invented unsigned block needs ir.stream.write", func(t *testing.T) {
		returned := append([]*pbv2.StreamEvent{textDelta("ok")},
			toolBlock(0, "call_1", "read_file", "", `{}`)...)
		if err := verifyStream(accepted, returned, noGrants); err == nil ||
			!strings.Contains(err.Error(), streamWriteGrant) || !strings.Contains(err.Error(), "0") {
			t.Fatalf("invented unsigned block without grant: err = %v, want grant-gated rejection", err)
		}
		if err := verifyStream(accepted, returned, withStreamWrite); err != nil {
			t.Fatalf("invented unsigned block with ir.stream.write rejected: %v", err)
		}
	})
	t.Run("invented signed block is added even with every grant", func(t *testing.T) {
		returned := append([]*pbv2.StreamEvent{textDelta("ok")},
			toolBlock(0, "call_1", "read_file", streamSigA, `{}`)...)
		err := verifyStream(accepted, returned, withEveryGrant)
		if err == nil {
			t.Fatal("invented signed block passed with every grant")
		}
		if !strings.Contains(err.Error(), "added") {
			t.Fatalf("invented signed block: err = %v, want added", err)
		}
	})
}

// TestValidateAcceptedStream pins the accepted-side split (round-1 decision
// 1): malformed ACCEPTED input is a host/adaptor defect, not a plugin
// failure. Every check here returns an *acceptedStreamError: unbound
// signature_delta, missing stop at successful completion, incompatible
// deltas, and events after StreamError.
func TestValidateAcceptedStream(t *testing.T) {
	valid := []struct {
		name   string
		events []*pbv2.StreamEvent
	}{
		{"empty stream", nil},
		{"bare text deltas", []*pbv2.StreamEvent{textDelta("hi"), textDelta(" there")}},
		{"bare text deltas closed by a signature", []*pbv2.StreamEvent{textDelta("hi"), signatureDelta(streamSigA)}},
		{"explicit text block", textBlock(0, "a", "b")},
		{"explicit thinking block", thinkingBlock(0, "r")},
		{"signed tool block", toolBlock(0, "call_1", "read_file", streamSigA, `{"path":"/a"}`)},
		{"interleaved concurrent tools", outboundpolicy.SignatureStreamFixtures()[12].Accepted},
		{"stream ending with StreamError", []*pbv2.StreamEvent{textDelta("a"), streamErrorEvent()}},
	}
	for _, tc := range valid {
		t.Run("valid: "+tc.name, func(t *testing.T) {
			if err := validateAcceptedStream(tc.events); err != nil {
				t.Fatalf("valid accepted stream rejected: %v", err)
			}
		})
	}

	// Every SDK fixture's accepted side must validate: verifyStream relies on
	// a valid baseline, and the fixtures are the cross-repo contract.
	for _, f := range outboundpolicy.SignatureStreamFixtures() {
		if err := validateAcceptedStream(f.Accepted); err != nil {
			t.Fatalf("fixture %q accepted stream rejected: %v", f.Name, err)
		}
	}

	invalid := []struct {
		name    string
		events  []*pbv2.StreamEvent
		wantMsg string
	}{
		{
			name:    "unbound signature_delta (leading)",
			events:  []*pbv2.StreamEvent{signatureDelta(streamSigA)},
			wantMsg: "no covered content",
		},
		{
			name: "unbound signature_delta after tool-call-only content",
			events: append(
				toolBlock(0, "call_1", "read_file", "", `{}`),
				signatureDelta(streamSigA)),
			wantMsg: "no covered content",
		},
		{
			name: "missing stop: explicit text block left open",
			events: []*pbv2.StreamEvent{
				{Event: &pbv2.StreamEvent_ContentBlockStart{ContentBlockStart: &pbv2.ContentBlockStart{
					Index: 0, Block: &pbv2.ContentBlockStart_Text{Text: &pbv2.TextBlock{}},
				}}},
				textDelta("a"),
			},
			wantMsg: "missing ContentBlockStop",
		},
		{
			name:    "missing stop: tool block left open",
			events:  []*pbv2.StreamEvent{toolStartEvent(0, "call_1", "read_file"), {Event: &pbv2.StreamEvent_ToolCallDelta{ToolCallDelta: &pbv2.ToolCallDelta{Index: 0, ArgumentsDelta: `{}`}}}},
			wantMsg: "missing ContentBlockStop",
		},
		{
			name: "text delta inside a thinking block",
			events: []*pbv2.StreamEvent{
				{Event: &pbv2.StreamEvent_ContentBlockStart{ContentBlockStart: &pbv2.ContentBlockStart{
					Index: 0, Block: &pbv2.ContentBlockStart_Thinking{Thinking: &pbv2.ThinkingBlock{}},
				}}},
				textDelta("x"),
			},
			wantMsg: "inside a thinking block",
		},
		{
			name: "thinking delta inside a text block",
			events: []*pbv2.StreamEvent{
				{Event: &pbv2.StreamEvent_ContentBlockStart{ContentBlockStart: &pbv2.ContentBlockStart{
					Index: 0, Block: &pbv2.ContentBlockStart_Text{Text: &pbv2.TextBlock{}},
				}}},
				thinkingDelta("x"),
			},
			wantMsg: "inside a text block",
		},
		{
			name:    "text delta inside an open tool block",
			events:  []*pbv2.StreamEvent{toolStartEvent(0, "call_1", "read_file"), textDelta("x")},
			wantMsg: "while a tool block is open",
		},
		{
			name:    "tool call delta naming no open tool block",
			events:  []*pbv2.StreamEvent{{Event: &pbv2.StreamEvent_ToolCallDelta{ToolCallDelta: &pbv2.ToolCallDelta{Index: 3, ArgumentsDelta: `{}`}}}},
			wantMsg: "names no open tool block",
		},
		{
			name:    "content block stop naming no open block",
			events:  []*pbv2.StreamEvent{{Event: &pbv2.StreamEvent_ContentBlockStop{ContentBlockStop: &pbv2.ContentBlockStop{Index: 0}}}},
			wantMsg: "names no open block",
		},
		{
			name:    "events after StreamError",
			events:  []*pbv2.StreamEvent{streamErrorEvent(), textDelta("after the error")},
			wantMsg: "after StreamError",
		},
	}
	for _, tc := range invalid {
		t.Run("invalid: "+tc.name, func(t *testing.T) {
			err := validateAcceptedStream(tc.events)
			if err == nil {
				t.Fatal("malformed accepted stream validated")
			}
			if !isAcceptedErr(err) {
				t.Fatalf("error is not an *acceptedStreamError: %T %v", err, err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error does not mention %q: %v", tc.wantMsg, err)
			}
		})
	}
}

// TestStreamVerifyAcceptedSideSeparation pins that verifyStream keeps the two
// error domains apart: a malformed ACCEPTED stream yields an
// *acceptedStreamError (host defect) no matter what the plugin returned, while
// a malformed signature_delta in the plugin's OUTPUT is a plain violation.
func TestStreamVerifyAcceptedSideSeparation(t *testing.T) {
	acceptedBad := []*pbv2.StreamEvent{signatureDelta(streamSigA)} // unbound: host defect
	returnedBad := []*pbv2.StreamEvent{signatureDelta(streamSigA)} // unbound: plugin violation

	err := verifyStream(acceptedBad, returnedBad, noGrants)
	if err == nil || !isAcceptedErr(err) {
		t.Fatalf("verifyStream over malformed accepted input: err = %v, want acceptedStreamError", err)
	}

	good := []*pbv2.StreamEvent{textDelta("ok")}
	err = verifyStream(good, returnedBad, noGrants)
	if err == nil || isAcceptedErr(err) {
		t.Fatalf("plugin-output unbound signature_delta: err = %v, want a plain violation", err)
	}
	if !strings.Contains(err.Error(), "no covered content") {
		t.Fatalf("plugin-output unbound error does not name the defect: %v", err)
	}

	// The accepted-side defect is detected even when the plugin would
	// otherwise pass: the host must fix its adapter, not the plugin.
	err = verifyStream(acceptedBad, good, noGrants)
	if err == nil || !isAcceptedErr(err) {
		t.Fatalf("malformed accepted input with clean output: err = %v, want acceptedStreamError", err)
	}
}

// TestStreamVerifyToolBlockSuppression pins the recorded host obligation:
// suppressing a signed tool block entirely is topology, allowed with
// ir.stream.write and rejected without (the error names the grant and the
// block). Suppressing an UNSIGNED accepted block is pure topology — the full
// field-policy walk's business — and passes here by design.
func TestStreamVerifyToolBlockSuppression(t *testing.T) {
	accepted := append([]*pbv2.StreamEvent{textDelta("ok")}, toolBlock(0, "call_1", "read_file", streamSigA, `{"path":"/a"}`)...)
	returned := []*pbv2.StreamEvent{textDelta("ok")}

	if err := verifyStream(accepted, returned, noGrants); err == nil {
		t.Fatal("suppressing a signed tool block without ir.stream.write passed")
	} else if !strings.Contains(err.Error(), streamWriteGrant) || !strings.Contains(err.Error(), "0") {
		t.Fatalf("error does not name the grant and the block index: %v", err)
	}
	if err := verifyStream(accepted, returned, withStreamWrite); err != nil {
		t.Fatalf("suppressing a signed tool block with ir.stream.write rejected: %v", err)
	}

	unsigned := append([]*pbv2.StreamEvent{textDelta("ok")}, toolBlock(0, "call_1", "read_file", "", `{}`)...)
	if err := verifyStream(unsigned, []*pbv2.StreamEvent{textDelta("ok")}, noGrants); err != nil {
		t.Fatalf("suppressing an unsigned accepted block is out of scope but was rejected: %v", err)
	}
}

// TestStreamVerifyCurrentBlockMatrix pins the CurrentContentBlock
// signature_delta matrix over the boundary-less host representation: content
// changed with the token kept is stale (rejected), changed with the token
// cleared is allowed, dropped without change is rejected, identical is intact,
// and token replacement/minting are forged/added.
func TestStreamVerifyCurrentBlockMatrix(t *testing.T) {
	accepted := []*pbv2.StreamEvent{textDelta("the original text"), signatureDelta(streamSigA)}

	t.Run("identical re-emission is intact", func(t *testing.T) {
		if err := verifyStream(accepted, accepted, noGrants); err != nil {
			t.Fatalf("identical stream rejected: %v", err)
		}
	})
	t.Run("content changed, token kept is stale", func(t *testing.T) {
		returned := []*pbv2.StreamEvent{textDelta("rewritten text"), signatureDelta(streamSigA)}
		err := verifyStream(accepted, returned, noGrants)
		if err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("kept token over changed content: err = %v, want stale", err)
		}
	})
	t.Run("content changed, token cleared is allowed", func(t *testing.T) {
		returned := []*pbv2.StreamEvent{textDelta("rewritten text")}
		if err := verifyStream(accepted, returned, noGrants); err != nil {
			t.Fatalf("cleared token over changed content rejected: %v", err)
		}
	})
	t.Run("token dropped without change is dropped", func(t *testing.T) {
		returned := []*pbv2.StreamEvent{textDelta("the original text")}
		err := verifyStream(accepted, returned, noGrants)
		if err == nil || !strings.Contains(err.Error(), "dropped") {
			t.Fatalf("token stripped from unchanged content: err = %v, want dropped", err)
		}
	})
	t.Run("token replaced is forged", func(t *testing.T) {
		returned := []*pbv2.StreamEvent{textDelta("the original text"), signatureDelta(streamSigB)}
		err := verifyStream(accepted, returned, noGrants)
		if err == nil || !strings.Contains(err.Error(), "forged") {
			t.Fatalf("token replaced: err = %v, want forged", err)
		}
	})
	t.Run("token minted where none was is added", func(t *testing.T) {
		unsigned := []*pbv2.StreamEvent{textDelta("the original text")}
		err := verifyStream(unsigned, accepted, noGrants)
		if err == nil || !strings.Contains(err.Error(), "added") {
			t.Fatalf("token minted: err = %v, want added", err)
		}
	})
	t.Run("same matrix over an explicit text block", func(t *testing.T) {
		explicit := append(textBlock(0, "inside a block"), signatureDelta(streamSigA))
		if err := verifyStream(explicit, explicit, noGrants); err != nil {
			t.Fatalf("identical explicit-block stream rejected: %v", err)
		}
		changed := append(textBlock(0, "changed"), signatureDelta(streamSigA))
		if err := verifyStream(explicit, changed, noGrants); err == nil ||
			!strings.Contains(err.Error(), "stale") {
			t.Fatalf("explicit block stale: err = %v", err)
		}
	})
}

// TestStreamVerifyCurrentBlockExplicitClearMarker pins that a plugin may
// represent clearing as an explicit empty signature_delta:
// ClassifySignatureMutation still decides by whether the covered content
// changed — and the exact (typed-content) match-first rule catches the
// explicit empty marker over UNCHANGED content as dropped even when the
// plugin moved it out of order.
func TestStreamVerifyCurrentBlockExplicitClearMarker(t *testing.T) {
	accepted := []*pbv2.StreamEvent{textDelta("original"), signatureDelta(streamSigA)}

	changed := []*pbv2.StreamEvent{textDelta("rewritten"), signatureDelta("")}
	if err := verifyStream(accepted, changed, noGrants); err != nil {
		t.Fatalf("explicit empty token over changed content rejected: %v", err)
	}
	unchanged := []*pbv2.StreamEvent{textDelta("original"), signatureDelta("")}
	if err := verifyStream(accepted, unchanged, noGrants); err == nil ||
		!strings.Contains(err.Error(), "dropped") {
		t.Fatalf("explicit empty token over unchanged content: err = %v, want dropped", err)
	}
}

// TestStreamVerifyTrailingMatrix pins the TrailingStandalone matrix over the
// preceding closed text/thinking content, plus the suppression gate for a
// signed text block.
func TestStreamVerifyTrailingMatrix(t *testing.T) {
	// accepted: one closed text block, then Code Assist's trailing
	// signature-only part.
	accepted := append(textBlock(0, "earlier text"), signatureDelta(streamSigA))

	t.Run("identical re-emission is intact", func(t *testing.T) {
		if err := verifyStream(accepted, accepted, noGrants); err != nil {
			t.Fatalf("identical stream rejected: %v", err)
		}
	})
	t.Run("closed content changed, token kept is stale", func(t *testing.T) {
		returned := append(textBlock(0, "rewritten"), signatureDelta(streamSigA))
		err := verifyStream(accepted, returned, noGrants)
		if err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("kept trailing token over changed closed content: err = %v, want stale", err)
		}
	})
	t.Run("closed content changed, token cleared is allowed", func(t *testing.T) {
		returned := textBlock(0, "rewritten")
		if err := verifyStream(accepted, returned, noGrants); err != nil {
			t.Fatalf("cleared trailing token over changed content rejected: %v", err)
		}
	})
	t.Run("token dropped without change is dropped", func(t *testing.T) {
		returned := textBlock(0, "earlier text")
		err := verifyStream(accepted, returned, noGrants)
		if err == nil || !strings.Contains(err.Error(), "dropped") {
			t.Fatalf("trailing token stripped from unchanged content: err = %v, want dropped", err)
		}
	})
	t.Run("token replaced is forged", func(t *testing.T) {
		returned := append(textBlock(0, "earlier text"), signatureDelta(streamSigB))
		err := verifyStream(accepted, returned, noGrants)
		if err == nil || !strings.Contains(err.Error(), "forged") {
			t.Fatalf("trailing token replaced: err = %v, want forged", err)
		}
	})
	t.Run("token minted where none was is added", func(t *testing.T) {
		unsigned := textBlock(0, "earlier text")
		err := verifyStream(unsigned, accepted, noGrants)
		if err == nil || !strings.Contains(err.Error(), "added") {
			t.Fatalf("trailing token minted: err = %v, want added", err)
		}
	})
	t.Run("suppressing the whole signed text block is topology-gated", func(t *testing.T) {
		if err := verifyStream(accepted, nil, noGrants); err == nil ||
			!strings.Contains(err.Error(), streamWriteGrant) {
			t.Fatalf("suppressed signed text block without grant: err = %v", err)
		}
		if err := verifyStream(accepted, nil, withStreamWrite); err != nil {
			t.Fatalf("suppressed signed text block with grant rejected: %v", err)
		}
	})
}

// TestStreamVerifyCurrentBlockSuppression pins the same suppression gate for
// a current-block signature: a signed text/thinking block that disappears is
// topology, not a cleared token.
func TestStreamVerifyCurrentBlockSuppression(t *testing.T) {
	accepted := []*pbv2.StreamEvent{textDelta("signed text"), signatureDelta(streamSigA)}

	if err := verifyStream(accepted, nil, noGrants); err == nil ||
		!strings.Contains(err.Error(), streamWriteGrant) {
		t.Fatalf("suppressed signed text block without grant: err = %v", err)
	}
	if err := verifyStream(accepted, nil, withStreamWrite); err != nil {
		t.Fatalf("suppressed signed text block with grant rejected: %v", err)
	}
}

// TestStreamVerifyUnboundSignatures pins the plugin-output side of the unbound
// rule: a signature_delta with no covered content in the plugin's OUTPUT — a
// floating token the plugin could mint — is a violation. (The same shape in
// ACCEPTED input is a host defect, covered by TestValidateAcceptedStream.)
func TestStreamVerifyUnboundSignatures(t *testing.T) {
	// The accepted side carries the same tool block (and NO text) so the only
	// defect in the plugin's output is the floating signature_delta after
	// tool-call-only content — which has no covered content at all.
	accepted := toolBlock(0, "call_1", "read_file", "", `{}`)

	for name, returned := range map[string][]*pbv2.StreamEvent{
		"after tool-call-only content": append(append([]*pbv2.StreamEvent{}, accepted...), signatureDelta(streamSigA)),
		"leading the stream":           {signatureDelta(streamSigA)},
	} {
		t.Run(name, func(t *testing.T) {
			err := verifyStream(accepted, returned, noGrants)
			if err == nil || !strings.Contains(err.Error(), "no covered content") {
				t.Fatalf("unbound signature_delta in output: err = %v, want rejection naming no covered content", err)
			}
			if isAcceptedErr(err) {
				t.Fatalf("plugin-output unbound signature_delta misclassified as an accepted-stream defect: %v", err)
			}
		})
	}
	inside := []*pbv2.StreamEvent{toolStartEvent(0, "c1", "read_file"), signatureDelta(streamSigA)}
	if err := verifyStream(accepted, inside, noGrants); err == nil ||
		!strings.Contains(err.Error(), "no covered content") {
		t.Fatalf("signature inside an open tool block: err = %v, want rejection", err)
	}
}

// TestStreamVerifyMultiSpanCorrelation pins the per-scope, per-value
// correlation across two current-block signatures: clearing the second token
// is allowed, dropping it is not, and the two blocks are judged independently.
func TestStreamVerifyMultiSpanCorrelation(t *testing.T) {
	accepted := []*pbv2.StreamEvent{
		textDelta("first"), signatureDelta(streamSigA),
		textDelta("second"), signatureDelta(streamSigB),
	}

	t.Run("clearing the second token over changed content is allowed", func(t *testing.T) {
		returned := []*pbv2.StreamEvent{
			textDelta("first"), signatureDelta(streamSigA),
			textDelta("second rewritten"),
		}
		if err := verifyStream(accepted, returned, noGrants); err != nil {
			t.Fatalf("cleared second token rejected: %v", err)
		}
	})
	t.Run("dropping the second token is dropped", func(t *testing.T) {
		returned := []*pbv2.StreamEvent{
			textDelta("first"), signatureDelta(streamSigA),
			textDelta("second"),
		}
		err := verifyStream(accepted, returned, noGrants)
		if err == nil || !strings.Contains(err.Error(), "dropped") {
			t.Fatalf("dropped second token: err = %v, want dropped", err)
		}
	})
	t.Run("changing the first span makes its kept token stale", func(t *testing.T) {
		returned := []*pbv2.StreamEvent{
			textDelta("first changed"), signatureDelta(streamSigA),
			textDelta("second"), signatureDelta(streamSigB),
		}
		err := verifyStream(accepted, returned, noGrants)
		if err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("first span changed with token kept: err = %v, want stale", err)
		}
	})
	t.Run("re-framing both spans into one suppresses the second binding", func(t *testing.T) {
		// The plugin merges the two text runs and drops both tokens. The
		// first binding's content no longer matches a whole returned span
		// (cleared over re-framed content), and the second binding's ordinal
		// is gone — a suppressed signed block, topology-gated. Both tokens
		// over surviving text are hostile, so rejection without the grant is
		// the safe outcome; with the grant this framing passes (documented
		// boundary: dropping-vs-suppressing are wire-indistinguishable here).
		returned := []*pbv2.StreamEvent{textDelta("firstsecond")}
		if err := verifyStream(accepted, returned, noGrants); err == nil ||
			!strings.Contains(err.Error(), streamWriteGrant) {
			t.Fatalf("re-framed drop without grant: err = %v, want topology rejection", err)
		}
		if err := verifyStream(accepted, returned, withStreamWrite); err != nil {
			t.Fatalf("re-framed drop with grant rejected: %v", err)
		}
	})
}

// TestStreamVerifyCrossIndexIdenticalFacts pins round-2 F1: after exact
// full-fact matching, tool blocks are correlated one-to-one ACROSS indexes by
// their IDENTICAL covered facts (id, name, assembled arguments), and the
// token is classified over the unchanged content. A coherent reindex (facts
// AND token moving together) is still intact at this layer, but a reindex
// with the token stripped is a dropped signature — the same verdict as the
// same-index form — and a reindex with the token replaced is forged. Neither
// is topology: ir.stream.write must not be able to erase signature/content
// correlation, so both are rejected even with every grant.
func TestStreamVerifyCrossIndexIdenticalFacts(t *testing.T) {
	accepted := toolBlock(0, "call_1", "read_file", streamSigA, `{"path":"/a"}`)

	t.Run("reindex with the token stripped is dropped, even with every grant", func(t *testing.T) {
		// The exact round-2 F1 reproduction: the same unchanged signed call
		// at a different index, token gone. The accepted block would be
		// read as invented-unsigned and suppressed-signed (both grantable);
		// identical facts must instead identify the block and classify the
		// missing token as dropped.
		returned := toolBlock(7, "call_1", "read_file", "", `{"path":"/a"}`)
		err := verifyStream(accepted, returned, withEveryGrant)
		if err == nil {
			t.Fatal("reindex-with-stripped-token passed with every grant")
		}
		if !strings.Contains(err.Error(), "dropped") {
			t.Fatalf("reindex-with-stripped-token: err = %v, want dropped", err)
		}
	})
	t.Run("reindex with the token replaced is forged", func(t *testing.T) {
		returned := toolBlock(7, "call_1", "read_file", streamSigB, `{"path":"/a"}`)
		err := verifyStream(accepted, returned, withEveryGrant)
		if err == nil || !strings.Contains(err.Error(), "forged") {
			t.Fatalf("reindex with token replaced: err = %v, want forged", err)
		}
	})
	t.Run("the same-index form is dropped too", func(t *testing.T) {
		returned := toolBlock(0, "call_1", "read_file", "", `{"path":"/a"}`)
		err := verifyStream(accepted, returned, withEveryGrant)
		if err == nil || !strings.Contains(err.Error(), "dropped") {
			t.Fatalf("same-index stripped token: err = %v, want dropped", err)
		}
	})
	t.Run("reindex with the token intact is still a coherent pass", func(t *testing.T) {
		returned := toolBlock(7, "call_1", "read_file", streamSigA, `{"path":"/a"}`)
		if err := verifyStream(accepted, returned, noGrants); err != nil {
			t.Fatalf("coherent reindex rejected: %v", err)
		}
	})
}

// TestStreamVerifyEmptyMarkerOwnIdentity pins round-2 F2: an explicit empty
// returned signature_delta is a clear marker belonging to the returned SPAN
// it was emitted in — never a pool of interchangeable markers consumed
// positionally. Every UNCHANGED accepted signed content occurrence surviving
// anywhere without its token is rejected as dropped BEFORE any clear marker
// is assigned, so a marker attached to a different (rewritten) span cannot
// hide a dropped token. The pinned shape: accepted block A signed T1 and
// block B signed T2; returned A unchanged with no token and B rewritten with
// an empty marker — previously consumed as "cleared" with no grants at all.
func TestStreamVerifyEmptyMarkerOwnIdentity(t *testing.T) {
	// Explicit blocks so the spans keep their identity across the rewrite
	// (the boundary-less representation would re-frame them into one span,
	// which is the separately-pinned re-framing case).
	accepted := append(signedTextBlock(0, "A-content", streamSigA),
		signedTextBlock(1, "B-content", streamSigB)...)

	t.Run("unchanged A without its token is dropped before B's marker is assigned", func(t *testing.T) {
		returned := append(textBlock(0, "A-content"),
			signedTextBlock(1, "B-rewritten", "")...)
		err := verifyStream(accepted, returned, noGrants)
		if err == nil || !strings.Contains(err.Error(), "dropped") {
			t.Fatalf("two-block regression: err = %v, want dropped", err)
		}
		if err := verifyStream(accepted, returned, withEveryGrant); err == nil ||
			!strings.Contains(err.Error(), "dropped") {
			t.Fatalf("two-block regression with every grant: err = %v, want dropped", err)
		}
	})
	t.Run("reversed order hides nothing either", func(t *testing.T) {
		// The rewritten span with its marker comes first, the unchanged
		// block second; the marker's own identity (returned span 0) would
		// point at the rewritten span, and the unchanged A still survives
		// without its token — dropped, order notwithstanding.
		returned := append(signedTextBlock(0, "B-rewritten", ""),
			textBlock(1, "A-content")...)
		if err := verifyStream(accepted, returned, withEveryGrant); err == nil ||
			!strings.Contains(err.Error(), "dropped") {
			t.Fatalf("reversed two-block regression: err = %v, want dropped", err)
		}
	})
	t.Run("both blocks rewritten, each with its own marker, clears both", func(t *testing.T) {
		returned := append(signedTextBlock(0, "A-rewritten", ""),
			signedTextBlock(1, "B-rewritten", "")...)
		if err := verifyStream(accepted, returned, noGrants); err != nil {
			t.Fatalf("both rewritten with their own markers rejected: %v", err)
		}
	})
	t.Run("A kept with its token, B rewritten with its own marker, clears B", func(t *testing.T) {
		returned := append(signedTextBlock(0, "A-content", streamSigA),
			signedTextBlock(1, "B-rewritten", "")...)
		if err := verifyStream(accepted, returned, noGrants); err != nil {
			t.Fatalf("legitimate clear rejected: %v", err)
		}
	})
}

// TestStreamVerifyExplicitEmptySignedBlock pins round-2 F3: an explicit
// text/thinking block opened and closed with ZERO deltas is still a span with
// empty typed content — the block exists even without content. An empty
// signed block whose token disappears while the block survives is a dropped
// signature, not a suppressed block: rejected even with every grant, with or
// without an explicit empty clear marker. Only when the whole block vanishes
// is it suppression (topology-gated).
func TestStreamVerifyExplicitEmptySignedBlock(t *testing.T) {
	emptyText := func(sig string, marker bool) []*pbv2.StreamEvent {
		out := []*pbv2.StreamEvent{{Event: &pbv2.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pbv2.ContentBlockStart{
				Index: 0, Block: &pbv2.ContentBlockStart_Text{Text: &pbv2.TextBlock{}},
			},
		}}}
		if marker {
			out = append(out, signatureDelta(sig))
		}
		return append(out, &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStop{
			ContentBlockStop: &pbv2.ContentBlockStop{Index: 0},
		}})
	}
	emptyThinking := func(sig string, marker bool) []*pbv2.StreamEvent {
		out := []*pbv2.StreamEvent{{Event: &pbv2.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pbv2.ContentBlockStart{
				Index: 0, Block: &pbv2.ContentBlockStart_Thinking{Thinking: &pbv2.ThinkingBlock{}},
			},
		}}}
		if marker {
			out = append(out, signatureDelta(sig))
		}
		return append(out, &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStop{
			ContentBlockStop: &pbv2.ContentBlockStop{Index: 0},
		}})
	}

	acceptedText := emptyText(streamSigA, true)
	acceptedThinking := emptyThinking(streamSigA, true)

	for _, tc := range []struct {
		name     string
		accepted []*pbv2.StreamEvent
		returned []*pbv2.StreamEvent
	}{
		{"empty text block, no marker", acceptedText, emptyText("", false)},
		{"empty text block, explicit empty marker", acceptedText, emptyText("", true)},
		{"empty thinking block, no marker", acceptedThinking, emptyThinking("", false)},
		{"empty thinking block, explicit empty marker", acceptedThinking, emptyThinking("", true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The block is still present; only its signature disappeared.
			// SignatureDropped, not a suppressed signed block — grants must
			// not save it.
			err := verifyStream(tc.accepted, tc.returned, withEveryGrant)
			if err == nil {
				t.Fatal("empty signed block losing its token passed with every grant")
			}
			if !strings.Contains(err.Error(), "dropped") {
				t.Fatalf("empty signed block: err = %v, want dropped", err)
			}
		})
	}

	t.Run("suppressing the whole empty signed block is topology, not dropped", func(t *testing.T) {
		// The block itself disappears — a suppressed signed block, gated on
		// ir.stream.write. The empty span representation is what keeps this
		// distinguishable from a present empty block with a dropped token.
		if err := verifyStream(acceptedText, nil, noGrants); err == nil ||
			!strings.Contains(err.Error(), streamWriteGrant) {
			t.Fatalf("suppressed empty signed block without grant: err = %v, want topology rejection", err)
		}
		if err := verifyStream(acceptedText, nil, withStreamWrite); err != nil {
			t.Fatalf("suppressed empty signed block with grant rejected: %v", err)
		}
	})
}

// TestStreamVerifyEmptySignedSpanThenContent pins round-3 F1: a
// signature_delta emitted in an explicit text/thinking block with NO deltas
// yet covers an EMPTY span, and that span is materialized AT THE SIGNATURE
// EVENT — the empty signed scope owns its ordinal, and later deltas become
// the next span. The finding: accepted StartText(0)+Sig(T)+Text("later")+Stop;
// returned StartText(0)+Text("later")+Stop — the walk recorded no span for
// the signature (spanOpen was false, closeSpan appended nothing), so the
// accepted empty binding also recorded ordinal 0 and the later text became
// spans[0]; when T vanished, verifyUnpairedBinding compared the empty signed
// scope to "later", called it rewritten, and allowed the drop. The block and
// the empty signed scope both survived; only the token vanished. With the
// empty span materialized, the empty binding's ordinal points at an empty
// span that does not survive — dropped, not cleared, and not suppression
// (the block still exists), so no grant can save it. Pinned for TEXT and
// THINKING, each with a later signed span and a later UNSIGNED span variant,
// under withEveryGrant (all dropped, rejected).
func TestStreamVerifyEmptySignedSpanThenContent(t *testing.T) {
	textStart := func() *pbv2.StreamEvent {
		return &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pbv2.ContentBlockStart{
				Index: 0, Block: &pbv2.ContentBlockStart_Text{Text: &pbv2.TextBlock{}},
			},
		}}
	}
	thinkingStart := func() *pbv2.StreamEvent {
		return &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pbv2.ContentBlockStart{
				Index: 0, Block: &pbv2.ContentBlockStart_Thinking{Thinking: &pbv2.ThinkingBlock{}},
			},
		}}
	}

	// build renders the accepted stream (empty signature first, then a
	// "later" span, optionally signed) and the plugin's returned stream with
	// the EMPTY signature dropped but the later content kept.
	build := func(start func() *pbv2.StreamEvent, delta func(string) *pbv2.StreamEvent, laterSigned bool) (accepted, returned []*pbv2.StreamEvent) {
		accepted = []*pbv2.StreamEvent{start()}
		accepted = append(accepted, signatureDelta(streamSigA))
		accepted = append(accepted, delta("later"))
		if laterSigned {
			accepted = append(accepted, signatureDelta(streamSigB))
		}
		accepted = append(accepted, &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStop{
			ContentBlockStop: &pbv2.ContentBlockStop{Index: 0},
		}})

		returned = []*pbv2.StreamEvent{start()}
		returned = append(returned, delta("later"))
		if laterSigned {
			returned = append(returned, signatureDelta(streamSigB))
		}
		returned = append(returned, &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStop{
			ContentBlockStop: &pbv2.ContentBlockStop{Index: 0},
		}})
		return accepted, returned
	}

	t.Run("the walk gives the empty signed span its own ordinal", func(t *testing.T) {
		accepted, _ := build(textStart, textDelta, false)
		v := scanStreamSignatures(accepted)
		if len(v.spans) != 2 || !v.spans[0].isEmpty() || v.spans[1].text != "later" {
			t.Fatalf("walk spans = %+v, want [empty, later]", v.spans)
		}
		if len(v.bindings) != 1 || v.bindings[0].span != 0 || !v.bindings[0].content.isEmpty() {
			t.Fatalf("walk bindings = %+v, want empty binding at span 0", v.bindings)
		}
	})

	for _, tc := range []struct {
		name        string
		start       func() *pbv2.StreamEvent
		delta       func(string) *pbv2.StreamEvent
		laterSigned bool
	}{
		{"text, later unsigned span", textStart, textDelta, false},
		{"text, later signed span", textStart, textDelta, true},
		{"thinking, later unsigned span", thinkingStart, thinkingDelta, false},
		{"thinking, later signed span", thinkingStart, thinkingDelta, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accepted, returned := build(tc.start, tc.delta, tc.laterSigned)
			// The empty signed span's token vanished while the block (and its
			// later content) survived: dropped — never "cleared" by the later
			// content at ordinal 0, and never topology (grants must not save
			// it).
			err := verifyStream(accepted, returned, withEveryGrant)
			if err == nil {
				t.Fatal("empty signed span losing its token passed with every grant")
			}
			if !strings.Contains(err.Error(), "dropped") {
				t.Fatalf("empty signed span: err = %v, want dropped", err)
			}
		})
	}
}

// TestStreamVerifyToolGlobalExactFirst pins round-3 F2: tool Phase 1 is a
// TRUE GLOBAL exact-full-fact pass over ALL returned scopes with a consumed
// set, and only AFTER every exact occurrence is consumed do the same-index
// and cross-index fallback correlations run. The finding: accepted A/Ta@0 and
// B/Tb@1; returned B modified with its token cleared at index 0 and A intact
// at index 1. The per-returned-loop exact scan let the modified B consume A
// through same-index correlation first, and the intact A then compared
// against B and was condemned "forged" — the verdict flipped when the
// returned order was reversed. At the signature layer both orders are valid:
// A moved coherently (all facts intact) and B changed with its token cleared.
// Both output orders must now pass.
func TestStreamVerifyToolGlobalExactFirst(t *testing.T) {
	accepted := append(
		toolBlock(0, "call_1", "read_file", streamSigA, `{"path":"/a"}`),
		toolBlock(1, "call_2", "write_file", streamSigB, `{"path":"/b"}`)...)
	modifiedFirst := append(
		toolBlock(0, "call_2", "write_file", "", `{"path":"/b-changed"}`),
		toolBlock(1, "call_1", "read_file", streamSigA, `{"path":"/a"}`)...)
	exactFirst := append(
		toolBlock(1, "call_1", "read_file", streamSigA, `{"path":"/a"}`),
		toolBlock(0, "call_2", "write_file", "", `{"path":"/b-changed"}`)...)

	for _, tc := range []struct {
		name     string
		returned []*pbv2.StreamEvent
	}{
		{"modified block first", modifiedFirst},
		{"exact match first", exactFirst},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A is consumed by the global exact pass; B' (changed content,
			// cleared token) is an invented UNSIGNED block at this layer and
			// needs ir.stream.write.
			if err := verifyStream(accepted, tc.returned, withStreamWrite); err != nil {
				t.Fatalf("order-dependent forged verdict: %v", err)
			}
			if err := verifyStream(accepted, tc.returned, noGrants); err == nil ||
				!strings.Contains(err.Error(), streamWriteGrant) {
				t.Fatalf("modified unsigned block without grant: err = %v, want grant-gated rejection", err)
			}
		})
	}
}

// TestStreamVerifyUnsignedTwinSuppression pins round-3 F3: the
// unchanged-content precheck is occurrence-aware multiset consumption. The
// finding: accepted signed block A/T + unsigned block A; returned unsigned
// block A (grant ir.stream.write) — the signed occurrence was suppressed
// (allowed with the topology grant) while the identical unsigned occurrence
// survived, but the old "survives ANYWHERE" check treated the surviving
// unsigned A as proof T was stripped and rejected the drop. Consumption is
// now one-to-one: exact intact signed bindings consume first (phase 1);
// surviving unchanged returned spans are matched against accepted UNSIGNED
// twins before they can prove a signed occurrence dropped; a SURPLUS
// unchanged returned occurrence corresponding to an unconsumed signed
// binding is dropped; and an unmatched signed accepted occurrence (content
// survived only as a twin) is suppression, gated on ir.stream.write.
func TestStreamVerifyUnsignedTwinSuppression(t *testing.T) {
	// signed+unsigned twin deletion in BOTH orders: the signed span is
	// suppressed and the identical unsigned twin survives as the single
	// returned block. Passes with ir.stream.write, rejected without — the
	// surviving twin must not be read as a stripped signed token.
	for name, accepted := range map[string][]*pbv2.StreamEvent{
		"signed twin first":   append(signedTextBlock(0, "A-content", streamSigA), textBlock(1, "A-content")...),
		"unsigned twin first": append(textBlock(0, "A-content"), signedTextBlock(1, "A-content", streamSigA)...),
	} {
		t.Run(name, func(t *testing.T) {
			returned := textBlock(0, "A-content")
			if err := verifyStream(accepted, returned, withStreamWrite); err != nil {
				t.Fatalf("twin deletion with grant rejected: %v", err)
			}
			if err := verifyStream(accepted, returned, noGrants); err == nil ||
				!strings.Contains(err.Error(), streamWriteGrant) {
				t.Fatalf("twin deletion without grant: err = %v, want topology rejection", err)
			}
		})
	}

	t.Run("without a twin, the same returned content is a dropped token", func(t *testing.T) {
		// Same returned shape, but the accepted stream has NO unsigned twin:
		// the single returned occurrence is the signed content surviving
		// without its token — dropped, even with every grant.
		accepted := signedTextBlock(0, "A-content", streamSigA)
		returned := textBlock(0, "A-content")
		err := verifyStream(accepted, returned, withEveryGrant)
		if err == nil || !strings.Contains(err.Error(), "dropped") {
			t.Fatalf("stripped signed content without twin: err = %v, want dropped", err)
		}
	})

	t.Run("two signed copies, one unsigned returned copy is surplus", func(t *testing.T) {
		// One returned occurrence cannot cover two signed accepted copies:
		// the signed content survives as a surplus occurrence → dropped.
		accepted := append(signedTextBlock(0, "A-content", streamSigA),
			signedTextBlock(1, "A-content", streamSigB)...)
		returned := textBlock(0, "A-content")
		err := verifyStream(accepted, returned, withEveryGrant)
		if err == nil || !strings.Contains(err.Error(), "dropped") {
			t.Fatalf("two signed copies, one unsigned returned copy: err = %v, want dropped", err)
		}
	})

	t.Run("twin plus surplus returned occurrences is dropped", func(t *testing.T) {
		// accepted: signed A, unsigned A, signed A; returned: TWO unsigned
		// A's. One returned occurrence is the surviving twin; the second is
		// a surplus occurrence proving a signed binding dropped.
		accepted := append(append(signedTextBlock(0, "A-content", streamSigA),
			textBlock(1, "A-content")...),
			signedTextBlock(2, "A-content", streamSigB)...)
		returned := append(textBlock(0, "A-content"), textBlock(1, "A-content")...)
		err := verifyStream(accepted, returned, withEveryGrant)
		if err == nil || !strings.Contains(err.Error(), "dropped") {
			t.Fatalf("twin plus surplus: err = %v, want dropped", err)
		}
	})
}

// TestStreamVerifyEmptyKindScopesDiffer pins round-4 F1: typedContent
// carries arm PRESENCE independently of bytes, so an empty TEXT scope and an
// empty THINKING scope are different records and can never match each other
// exactly. The finding: the explicit EMPTY kinds both produced the identical
// zero record {text:"", thinking:""}, so Phase 1 consumed accepted
// StartText(0)+Sig(T)+Stop vs returned StartThinking(0)+Sig(T)+Stop as an
// EXACT match and the token moved from an empty text scope to an empty
// thinking scope with every grant. Presence is now marked by the explicit
// block's kind and by every delta of the kind — INCLUDING empty deltas — so
// the empty kinds differ and the swap is classified stale, exactly like the
// non-empty text-A -> thinking-A case. Pinned for BOTH directions, for
// explicit ZERO-DELTA signed blocks and for explicit EMPTY-DELTA signed
// blocks (TextDelta("")/ThinkingDelta("")), under withEveryGrant — all
// stale, rejected — and unchanged empty blocks of each kind must still pass
// intact.
func TestStreamVerifyEmptyKindScopesDiffer(t *testing.T) {
	// emptyBlock renders an explicit text/thinking block whose content is a
	// single (possibly EMPTY) delta of the matching kind, with an optional
	// signature_delta inside — the four F1 shapes.
	emptyText := func(sig string, emptyDelta bool) []*pbv2.StreamEvent {
		out := []*pbv2.StreamEvent{{Event: &pbv2.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pbv2.ContentBlockStart{
				Index: 0, Block: &pbv2.ContentBlockStart_Text{Text: &pbv2.TextBlock{}},
			},
		}}}
		if emptyDelta {
			out = append(out, textDelta(""))
		}
		if sig != "" {
			out = append(out, signatureDelta(sig))
		}
		return append(out, &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStop{
			ContentBlockStop: &pbv2.ContentBlockStop{Index: 0},
		}})
	}
	emptyThinking := func(sig string, emptyDelta bool) []*pbv2.StreamEvent {
		out := []*pbv2.StreamEvent{{Event: &pbv2.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pbv2.ContentBlockStart{
				Index: 0, Block: &pbv2.ContentBlockStart_Thinking{Thinking: &pbv2.ThinkingBlock{}},
			},
		}}}
		if emptyDelta {
			out = append(out, thinkingDelta(""))
		}
		if sig != "" {
			out = append(out, signatureDelta(sig))
		}
		return append(out, &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStop{
			ContentBlockStop: &pbv2.ContentBlockStop{Index: 0},
		}})
	}

	// The token crossed the kind boundary between two EMPTY scopes; the
	// kinds differ, so this is stale — the same verdict as text-A ->
	// thinking-A — and no grant may save it.
	for _, tc := range []struct {
		name     string
		accepted []*pbv2.StreamEvent
		returned []*pbv2.StreamEvent
	}{
		{"zero-delta: empty text scope -> empty thinking scope", emptyText(streamSigA, false), emptyThinking(streamSigA, false)},
		{"zero-delta: empty thinking scope -> empty text scope", emptyThinking(streamSigA, false), emptyText(streamSigA, false)},
		{"empty-delta: empty text scope -> empty thinking scope", emptyText(streamSigA, true), emptyThinking(streamSigA, true)},
		{"empty-delta: empty thinking scope -> empty text scope", emptyThinking(streamSigA, true), emptyText(streamSigA, true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyStream(tc.accepted, tc.returned, withEveryGrant)
			if err == nil {
				t.Fatal("empty kind swap passed with every grant")
			}
			if !strings.Contains(err.Error(), "stale") {
				t.Fatalf("empty kind swap: err = %v, want stale", err)
			}
		})
	}

	// An unchanged empty block of each kind — zero-delta and empty-delta
	// forms — must still pass intact: the presence fields match, so Phase 1
	// consumes the exact (token, typed-content) match as before.
	for _, tc := range []struct {
		name   string
		stream func(sig string, emptyDelta bool) []*pbv2.StreamEvent
	}{
		{"empty text block intact", emptyText},
		{"empty thinking block intact", emptyThinking},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, emptyDelta := range []bool{false, true} {
				events := tc.stream(streamSigA, emptyDelta)
				if err := verifyStream(events, events, noGrants); err != nil {
					t.Fatalf("unchanged empty block (emptyDelta=%v) rejected: %v", emptyDelta, err)
				}
			}
		})
	}
}

// TestStreamVerifyExactMatchesReserveReturnedSpans pins round-4 F2: Phase 1
// consumes exact (token, typed-content) matches in consumed/retConsumed, but
// the Phase-3 remaining multiset was rebuilt from ALL returned spans and
// never subtracted the returned spans owned by those exact matches. The
// finding: accepted signed text "same"/T1 + signed text "same"/T2; returned
// signed text "same"/T1 with the grant ir.stream.write — suppressing T2's
// block is valid topology, but T1's returned "same" span stayed in the
// multiset and T2 claimed it, so verification returned dropped. The returned
// span ordinal of every exact-consumed current binding is now reserved from
// the multiset BEFORE unsigned twins (or anything else) may claim it; only a
// SURPLUS returned occurrence can then prove a dropped token. Pinned: the
// two-identical-signed case is nil with ir.stream.write and a suppression
// error without the grant, plus the empty-span equivalent.
func TestStreamVerifyExactMatchesReserveReturnedSpans(t *testing.T) {
	accepted := append(signedTextBlock(0, "same", streamSigA),
		signedTextBlock(1, "same", streamSigB)...)
	returned := signedTextBlock(0, "same", streamSigA)

	t.Run("identical signed occurrences: suppression is topology, not dropped", func(t *testing.T) {
		// T1's returned span is reserved by its exact match; T2 has no
		// returned counterpart, so its block was suppressed — valid with the
		// grant, a grant-gated violation without.
		if err := verifyStream(accepted, returned, withStreamWrite); err != nil {
			t.Fatalf("identical-signed suppression with grant rejected: %v", err)
		}
		if err := verifyStream(accepted, returned, noGrants); err == nil ||
			!strings.Contains(err.Error(), streamWriteGrant) {
			t.Fatalf("identical-signed suppression without grant: err = %v, want topology rejection", err)
		}
	})

	// The empty-span equivalent: the same two-identical-signed shape over
	// explicit EMPTY text blocks (zero deltas). T1's returned empty span is
	// reserved by the exact match; T2's block suppression is topology.
	emptySignedAt := func(index int32, sig string) []*pbv2.StreamEvent {
		return []*pbv2.StreamEvent{
			{Event: &pbv2.StreamEvent_ContentBlockStart{ContentBlockStart: &pbv2.ContentBlockStart{
				Index: index, Block: &pbv2.ContentBlockStart_Text{Text: &pbv2.TextBlock{}},
			}}},
			signatureDelta(sig),
			{Event: &pbv2.StreamEvent_ContentBlockStop{ContentBlockStop: &pbv2.ContentBlockStop{Index: index}}},
		}
	}
	emptyAccepted := append(emptySignedAt(0, streamSigA), emptySignedAt(1, streamSigB)...)
	emptyReturned := emptySignedAt(0, streamSigA)

	t.Run("identical empty signed occurrences: suppression is topology, not dropped", func(t *testing.T) {
		if err := verifyStream(emptyAccepted, emptyReturned, withStreamWrite); err != nil {
			t.Fatalf("empty identical-signed suppression with grant rejected: %v", err)
		}
		if err := verifyStream(emptyAccepted, emptyReturned, noGrants); err == nil ||
			!strings.Contains(err.Error(), streamWriteGrant) {
			t.Fatalf("empty identical-signed suppression without grant: err = %v, want topology rejection", err)
		}
	})
}

// TestValidateAcceptedStreamABITopology pins round-2 F4: validateAcceptedStream
// implements the FULL ABI topology, not a kind-only approximation. Every
// stop/delta must match its open block BY INDEX, indexes are never reused,
// non-tool blocks are exclusive and never overlap tool blocks, MessageStop
// with any open block is rejected immediately (a later stop cannot hide it),
// and after MessageStop only Usage (or a terminal StreamError) may follow.
// All failures are *acceptedStreamError and are propagated by verifyStream
// as host defects, never plugin violations.
func TestValidateAcceptedStreamABITopology(t *testing.T) {
	textStart := func(index int32) *pbv2.StreamEvent {
		return &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pbv2.ContentBlockStart{
				Index: index, Block: &pbv2.ContentBlockStart_Text{Text: &pbv2.TextBlock{}},
			},
		}}
	}
	textStop := func(index int32) *pbv2.StreamEvent {
		return &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStop{
			ContentBlockStop: &pbv2.ContentBlockStop{Index: index},
		}}
	}
	toolDelta := func(index int32, frag string) *pbv2.StreamEvent {
		return &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ToolCallDelta{
			ToolCallDelta: &pbv2.ToolCallDelta{Index: index, ArgumentsDelta: frag},
		}}
	}

	invalid := []struct {
		name    string
		events  []*pbv2.StreamEvent
		wantMsg string
	}{
		{
			name: "wrong non-tool stop index: StartText(3) closed by Stop(9)",
			events: []*pbv2.StreamEvent{
				textStart(3), textDelta("x"), textStop(9),
			},
			wantMsg: "names no open block",
		},
		{
			name: "MessageStop with an open tool block, stop hidden until after",
			events: []*pbv2.StreamEvent{
				toolStartEvent(0, "call_1", "read_file"),
				messageStopEvent("tool_calls"),
				textStop(0),
			},
			wantMsg: "MessageStop",
		},
		{
			name: "MessageStop with an open text block",
			events: []*pbv2.StreamEvent{
				textStart(0), textDelta("x"), messageStopEvent("stop"),
			},
			wantMsg: "MessageStop",
		},
		{
			name: "index reused after close",
			events: append(append([]*pbv2.StreamEvent{},
				toolBlock(0, "call_1", "read_file", "", `{}`)...),
				toolStartEvent(0, "call_2", "write_file")),
			wantMsg: "reused",
		},
		{
			name: "index reused while open",
			events: []*pbv2.StreamEvent{
				toolStartEvent(0, "call_1", "read_file"),
				toolStartEvent(0, "call_2", "write_file"),
			},
			wantMsg: "reused",
		},
		{
			name: "non-tool start while a tool block is open",
			events: []*pbv2.StreamEvent{
				toolStartEvent(0, "call_1", "read_file"),
				textStart(1),
			},
			wantMsg: "while a tool block is open",
		},
		{
			name: "tool start while a non-tool block is open",
			events: []*pbv2.StreamEvent{
				textStart(0), textStart(1),
			},
			wantMsg: "while a text block is open",
		},
		{
			name: "second non-tool block while one is open",
			events: []*pbv2.StreamEvent{
				textStart(0),
				{Event: &pbv2.StreamEvent_ContentBlockStart{ContentBlockStart: &pbv2.ContentBlockStart{
					Index: 1, Block: &pbv2.ContentBlockStart_Thinking{Thinking: &pbv2.ThinkingBlock{}},
				}}},
			},
			wantMsg: "while a text block is open",
		},
		{
			name: "content after MessageStop",
			events: []*pbv2.StreamEvent{
				toolStartEvent(0, "call_1", "read_file"), textStop(0),
				messageStopEvent("stop"), textDelta("too late"),
			},
			wantMsg: "after MessageStop",
		},
		{
			name: "tool delta after MessageStop",
			events: []*pbv2.StreamEvent{
				messageStopEvent("stop"), toolDelta(0, `{}`),
			},
			wantMsg: "after MessageStop",
		},
	}
	for _, tc := range invalid {
		t.Run("invalid: "+tc.name, func(t *testing.T) {
			err := validateAcceptedStream(tc.events)
			if err == nil {
				t.Fatal("malformed accepted stream validated")
			}
			if !isAcceptedErr(err) {
				t.Fatalf("error is not an *acceptedStreamError: %T %v", err, err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error does not mention %q: %v", tc.wantMsg, err)
			}
			// verifyStream must propagate the typed host defect unchanged,
			// no matter what the plugin returned.
			if err := verifyStream(tc.events, nil, withEveryGrant); err == nil || !isAcceptedErr(err) {
				t.Fatalf("verifyStream over malformed accepted input: err = %v, want acceptedStreamError", err)
			}
		})
	}

	valid := []struct {
		name   string
		events []*pbv2.StreamEvent
	}{
		{
			name: "usage after MessageStop is allowed",
			events: []*pbv2.StreamEvent{
				toolStartEvent(0, "call_1", "read_file"), textStop(0),
				messageStopEvent("stop"), usageEvent(),
			},
		},
		{
			name: "usage before MessageStop is allowed",
			events: []*pbv2.StreamEvent{
				toolStartEvent(0, "call_1", "read_file"), textStop(0),
				usageEvent(), messageStopEvent("stop"),
			},
		},
		{
			name: "concurrent tool blocks close before MessageStop",
			events: []*pbv2.StreamEvent{
				toolStartEvent(0, "call_1", "read_file"),
				toolStartEvent(1, "call_2", "write_file"),
				toolDelta(1, `{"b":1}`), toolDelta(0, `{"a":1}`),
				textStop(0), textStop(1),
				messageStopEvent("tool_calls"),
			},
		},
	}
	for _, tc := range valid {
		t.Run("valid: "+tc.name, func(t *testing.T) {
			if err := validateAcceptedStream(tc.events); err != nil {
				t.Fatalf("valid accepted stream rejected: %v", err)
			}
		})
	}
}

// BenchmarkVerifyStreamPassThrough measures the whole stream verification on
// a realistic signed accepted/returned stream (pass-through), at the repo's
// standard event sizes. This is the O(n) single-pass walk from round 1, and
// the shape is the same as the closed #243 branch's BenchmarkVerifyStream so
// the numbers are comparable. The follow-up (2b) production benchmark adds the
// state bookkeeping and boundary cadence of the per-event enforcement loop
// (grant lookup, full-walk field diff, rejection wiring — measured through
// the pipeline, as BenchmarkRunOnStreamChunk does); the pure verifier cost
// measured here is its inner core.
func BenchmarkVerifyStreamPassThrough(b *testing.B) {
	for _, n := range benchSizes {
		accepted := benchStream(n)
		returned := benchStream(n)
		b.Run(fmt.Sprintf("events=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if err := verifyStream(accepted, returned, noGrants); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkVerifyStreamManyTools measures the walk over MANY concurrently open
// tool blocks (20, the round-1 benchmark shape): per-open-tool builders keep
// the scopes apart in one pass, so the cost must stay near-linear in events.
func BenchmarkVerifyStreamManyTools(b *testing.B) {
	accepted := benchConcurrentTools(20)
	returned := benchConcurrentTools(20)
	b.Run("tools=20", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if err := verifyStream(accepted, returned, noGrants); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkVerifyStreamFragmented measures the walk over ONE tool call whose
// arguments arrive as 100 fragments (the round-1 benchmark shape): the
// builder accumulates into a single buffer and materializes once, so the cost
// must not depend on fragment-count squared.
func BenchmarkVerifyStreamFragmented(b *testing.B) {
	accepted := benchFragmentedTool(100)
	returned := benchFragmentedTool(100)
	b.Run("fragments=100", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if err := verifyStream(accepted, returned, noGrants); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// benchStream builds a realistic signed stream of n events: message framing,
// text chunks, one signed tool block, and Code Assist's trailing signature.
// Pass-through verification of it is the common-case cost. (Same shape as the
// closed #243 branch's helper, for comparable numbers.)
func benchStream(n int) []*pbv2.StreamEvent {
	messageStart := func() *pbv2.StreamEvent {
		return &pbv2.StreamEvent{Event: &pbv2.StreamEvent_MessageStart{
			MessageStart: &pbv2.MessageStart{Role: "assistant", Model: "gemini-2.5"},
		}}
	}
	messageStop := func() *pbv2.StreamEvent {
		return &pbv2.StreamEvent{Event: &pbv2.StreamEvent_MessageStop{
			MessageStop: &pbv2.MessageStop{FinishReason: "stop"},
		}}
	}
	if n <= 2 {
		out := make([]*pbv2.StreamEvent, 0, n)
		for i := range n {
			out = append(out, textDelta(fmt.Sprintf("token chunk %d ", i)))
		}
		return out
	}
	if n < 6 {
		out := make([]*pbv2.StreamEvent, 0, n)
		out = append(out, messageStart())
		for i := range n - 2 {
			out = append(out, textDelta(fmt.Sprintf("token chunk %d ", i)))
		}
		return append(out, messageStop())
	}
	out := make([]*pbv2.StreamEvent, 0, n)
	out = append(out, messageStart())
	for i := range n - 6 {
		out = append(out, textDelta(fmt.Sprintf("text chunk %d for the agent to verify ", i)))
	}
	out = append(out, toolBlock(0, "call_1", "read_file", streamSigA, `{"path":"/a"}`)...)
	out = append(out, signatureDelta("trailing-token"))
	return append(out, messageStop())
}

// benchConcurrentTools builds n tool-call blocks open CONCURRENTLY with their
// argument deltas interleaved by index, wrapped in message framing — the
// OpenAI Chat parallel-tool shape, which only an index-keyed single-pass
// assembler can keep apart.
func benchConcurrentTools(n int) []*pbv2.StreamEvent {
	out := []*pbv2.StreamEvent{{Event: &pbv2.StreamEvent_MessageStart{
		MessageStart: &pbv2.MessageStart{Role: "assistant", Model: "gemini-2.5"},
	}}}
	for i := range n {
		out = append(out, &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pbv2.ContentBlockStart{
				Index: int32(i),
				Block: &pbv2.ContentBlockStart_ToolCall{ToolCall: &pbv2.ToolCallRef{
					Id: fmt.Sprintf("call_%d", i), Name: "read_file", Signature: streamSigA,
				}},
			},
		}})
	}
	// Round-robin argument fragments so every block stays open across the
	// whole burst.
	for frag := range 3 {
		for i := range n {
			out = append(out, &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ToolCallDelta{
				ToolCallDelta: &pbv2.ToolCallDelta{Index: int32(i), ArgumentsDelta: fmt.Sprintf(`{"path":"/f%d`, frag)},
			}})
		}
	}
	for i := range n {
		out = append(out, &pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStop{
			ContentBlockStop: &pbv2.ContentBlockStop{Index: int32(i)},
		}})
	}
	return append(out, &pbv2.StreamEvent{Event: &pbv2.StreamEvent_MessageStop{
		MessageStop: &pbv2.MessageStop{FinishReason: "stop"},
	}})
}

// benchFragmentedTool builds one signed tool call whose arguments arrive as n
// fragments, wrapped in message framing.
func benchFragmentedTool(n int) []*pbv2.StreamEvent {
	args := make([]string, 0, n)
	for i := range n {
		args = append(args, fmt.Sprintf(`{"chunk":%d,`, i))
	}
	out := []*pbv2.StreamEvent{{Event: &pbv2.StreamEvent_MessageStart{
		MessageStart: &pbv2.MessageStart{Role: "assistant", Model: "gemini-2.5"},
	}}}
	out = append(out, toolBlockDeltas(0, "call_1", "read_file", streamSigA, args...)...)
	return append(out, &pbv2.StreamEvent{Event: &pbv2.StreamEvent_MessageStop{
		MessageStop: &pbv2.MessageStop{FinishReason: "stop"},
	}})
}
