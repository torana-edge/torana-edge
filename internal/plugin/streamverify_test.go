package plugin

import (
	"fmt"
	"strings"
	"testing"

	"github.com/torana-edge/torana-plugin-sdk/outboundpolicy"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// Tests for the stream signature verifier (Migration B part 2a).
//
// The scope definitions — CurrentContentBlock and TrailingStandalone over the
// event-walk in scanStreamSignatures — are pinned here directly
// (TestScanStreamSignaturesPinsTheScopeWalk) and through the behaviour
// matrices (TestStreamVerifyCurrentBlockMatrix, TestStreamVerifyTrailingMatrix).
//
// The SDK's SignatureStreamFixtures are the cross-repo transactional contract:
// a verifier is correct on the ToolCallBlockByIndex axis exactly when, given
// Accepted and Returned, its OWN scope diff plus ClassifySignatureMutation
// yields Want for the block at Index. TestVerifyStreamConformsToSDKStreamFixtures
// runs verifyStream over every fixture, and
// TestStreamVerifyWrongScopesFailTheFixtures proves the scope functions are
// not one of the three wrong implementations the fixtures exist to catch.

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

func noGrants(string) bool { return false }

func withStreamWrite(string) bool { return true }

// TestVerifyStreamConformsToSDKStreamFixtures runs the SDK's cross-repo
// transactional contract through verifyStream's signature axis: for every
// fixture, an Allowed Want means verifyStream must return nil, and a rejected
// Want means it must return an error naming the class.
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
// semantics: which signature_delta lands in which scope, with which covered
// content, in both the boundary-less host representation and the
// ABI-conformant explicit-block representation.
func TestScanStreamSignaturesPinsTheScopeWalk(t *testing.T) {
	// Boundary-less host representation (engine IR has no text-block
	// boundaries): the trailing Code Assist part is indistinguishable from a
	// same-part current signature, so it classifies as current over the whole
	// run — content-wise the same binding as the SDK's "preceding closed
	// text/thinking content of the turn".
	v := scanStreamSignatures([]*pbv2.StreamEvent{textDelta("t1"), textDelta("t2"), signatureDelta(streamSigA)})
	if len(v.bindings) != 1 || v.bindings[0].kind != sigBindingCurrent || v.bindings[0].content != "t1t2" {
		t.Fatalf("boundary-less run: bindings = %+v, want one current binding over t1t2", v.bindings)
	}

	// A signature ends the span it covers; a later part's text opens a fresh
	// span (the provider part model).
	v = scanStreamSignatures([]*pbv2.StreamEvent{
		textDelta("p1"), signatureDelta(streamSigA), textDelta("p2"), signatureDelta(streamSigB),
	})
	if len(v.bindings) != 2 {
		t.Fatalf("two parts: got %d bindings, want 2", len(v.bindings))
	}
	if v.bindings[0].kind != sigBindingCurrent || v.bindings[0].content != "p1" || v.bindings[0].span != 0 {
		t.Fatalf("first binding = %+v, want current over p1 at span 0", v.bindings[0])
	}
	if v.bindings[1].kind != sigBindingCurrent || v.bindings[1].content != "p2" || v.bindings[1].span != 1 {
		t.Fatalf("second binding = %+v, want current over p2 at span 1", v.bindings[1])
	}

	// ABI-conformant: signature inside the open block is current; signature
	// after the stop is trailing over the closed content. textBlock(0, "a")
	// is [start, delta, stop]; reorder to start, delta, signature, stop so the
	// signature sits inside the open block.
	open := []*pbv2.StreamEvent{
		{Event: &pbv2.StreamEvent_ContentBlockStart{ContentBlockStart: &pbv2.ContentBlockStart{
			Index: 0, Block: &pbv2.ContentBlockStart_Text{Text: &pbv2.TextBlock{}},
		}}},
		textDelta("a"),
		signatureDelta(streamSigA),
		{Event: &pbv2.StreamEvent_ContentBlockStop{ContentBlockStop: &pbv2.ContentBlockStop{Index: 0}}},
	}
	v = scanStreamSignatures(open)
	if len(v.bindings) != 1 || v.bindings[0].kind != sigBindingCurrent || v.bindings[0].content != "a" {
		t.Fatalf("sig inside open text block: %+v, want current over a", v.bindings)
	}
	closed := append(textBlock(0, "a"), signatureDelta(streamSigA))
	v = scanStreamSignatures(closed)
	if len(v.bindings) != 1 || v.bindings[0].kind != sigBindingTrailing || v.bindings[0].content != "a" {
		t.Fatalf("sig after closed text block: %+v, want trailing over a", v.bindings)
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
	v = scanStreamSignatures([]*pbv2.StreamEvent{
		{Event: &pbv2.StreamEvent_ContentBlockStart{ContentBlockStart: &pbv2.ContentBlockStart{
			Index: 0, Block: &pbv2.ContentBlockStart_ToolCall{ToolCall: &pbv2.ToolCallRef{Id: "c1", Name: "read_file"}},
		}}},
		signatureDelta(streamSigA),
	})
	if len(v.bindings) != 1 || v.bindings[0].kind != sigBindingUnbound {
		t.Fatalf("sig inside open tool block: %+v, want unbound", v.bindings)
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

// TestStreamVerifyInventedToolBlock pins the returned-only topology gate: a
// block the plugin invented changes stream cardinality and needs ir.stream.write.
func TestStreamVerifyInventedToolBlock(t *testing.T) {
	accepted := []*pbv2.StreamEvent{textDelta("ok")}
	returned := append([]*pbv2.StreamEvent{textDelta("ok")}, toolBlock(0, "call_1", "read_file", "", `{}`)...)

	if err := verifyStream(accepted, returned, noGrants); err == nil {
		t.Fatal("inventing a tool block without ir.stream.write passed")
	} else if !strings.Contains(err.Error(), streamWriteGrant) || !strings.Contains(err.Error(), "0") {
		t.Fatalf("error does not name the grant and the block index: %v", err)
	}
	if err := verifyStream(accepted, returned, withStreamWrite); err != nil {
		t.Fatalf("inventing a tool block with ir.stream.write rejected: %v", err)
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
// represent clearing as an explicit empty signature_delta: ClassifySignatureMutation
// still decides by whether the covered content changed.
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

// TestStreamVerifyUnboundSignatures pins the conservative reading of the SDK
// contract that signature_delta "does not bind tool-call blocks": a
// signature_delta with no covered content — after a tool-call-only turn, at
// the head of the stream, or inside an open tool block — is malformed and
// rejected. Flagged for review in the PR report.
func TestStreamVerifyUnboundSignatures(t *testing.T) {
	toolOnly := append(toolBlock(0, "call_1", "read_file", "", `{}`), signatureDelta(streamSigA))
	for name, stream := range map[string][]*pbv2.StreamEvent{
		"after tool-call-only content": toolOnly,
		"leading the stream":           {signatureDelta(streamSigA)},
	} {
		t.Run(name, func(t *testing.T) {
			err := verifyStream(stream, stream, noGrants)
			if err == nil || !strings.Contains(err.Error(), "no covered content") {
				t.Fatalf("unbound signature_delta: err = %v, want rejection naming no covered content", err)
			}
		})
	}
	inside := []*pbv2.StreamEvent{
		{Event: &pbv2.StreamEvent_ContentBlockStart{ContentBlockStart: &pbv2.ContentBlockStart{
			Index: 0, Block: &pbv2.ContentBlockStart_ToolCall{ToolCall: &pbv2.ToolCallRef{Id: "c1", Name: "read_file"}},
		}}},
		signatureDelta(streamSigA),
	}
	if err := verifyStream(inside, inside, noGrants); err == nil ||
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

// BenchmarkVerifyStream measures the whole stream verification on realistic
// signed accepted/returned streams (pass-through), at the repo's standard
// event sizes. The numbers gate turning enforcement on: the follow-up PR's
// per-plugin stream verification cost is verifyStream over each plugin's
// output, and it must be a small fraction of the per-event hook cost measured
// by BenchmarkRunOnStreamChunk.
func BenchmarkVerifyStream(b *testing.B) {
	for _, n := range benchSizes {
		accepted := benchStream(n)
		returned := benchStream(n)
		canWrite := noGrants
		b.Run(fmt.Sprintf("events=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if err := verifyStream(accepted, returned, canWrite); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// benchStream builds a realistic signed stream of n events: message framing,
// text chunks, one signed tool block, and Code Assist's trailing signature.
// Pass-through verification of it is the common-case cost.
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
