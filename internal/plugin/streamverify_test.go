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
