package plugin

// Tests for the stream-signature enforcement layer (Migration B part 2b):
// the per-event pre-commit discipline, the scope-close (late) checks, the
// accepted-side host-defect attribution, and the typed terminal error.
//
// The enforcement state machine is tested two ways:
//
//   - pure state tests drive streamVerifierState/pluginStreamState directly
//     with synthetic events (no WASM), pinning the pre-commit vs late
//     classification and the failure_mode decisions;
//   - end-to-end tests run RunOnStreamChunkVerified over real fixture
//     plugins, proving the enforcement fires (or does not fire) on the exact
//     production path.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// --- walker mirror ---------------------------------------------------------

// unwrapAccepted normalizes a discipline error for comparison: the walker
// returns bare messages while validateAcceptedStream wraps its msg in an
// *acceptedStreamError.
func unwrapAccepted(err error) string {
	var ae *acceptedStreamError
	if errors.As(err, &ae) {
		return ae.msg
	}
	return err.Error()
}

// TestStreamDisciplineWalkerMirrorsValidateAcceptedStream pins that the
// incremental walker implements validateAcceptedStream's per-event rules
// exactly: for every sequence, the walker's first error (fed one event at a
// time) matches validateAcceptedStream's error on the same whole sequence —
// same rule, same position, same text.
func TestStreamDisciplineWalkerMirrorsValidateAcceptedStream(t *testing.T) {
	seq := func(evs ...*pbv1.StreamEvent) []*pbv1.StreamEvent { return evs }
	text := func(s string) *pbv1.StreamEvent { return textDelta(s) }
	sig := func(s string) *pbv1.StreamEvent { return signatureDelta(s) }
	startText := func(idx int32) *pbv1.StreamEvent {
		return &pbv1.StreamEvent{Event: &pbv1.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pbv1.ContentBlockStart{Index: idx, Block: &pbv1.ContentBlockStart_Text{Text: &pbv1.TextBlock{}}},
		}}
	}
	startThinking := func(idx int32) *pbv1.StreamEvent {
		return &pbv1.StreamEvent{Event: &pbv1.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pbv1.ContentBlockStart{Index: idx, Block: &pbv1.ContentBlockStart_Thinking{Thinking: &pbv1.ThinkingBlock{}}},
		}}
	}
	startProvider := func(idx int32) *pbv1.StreamEvent {
		return &pbv1.StreamEvent{Event: &pbv1.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pbv1.ContentBlockStart{Index: idx, Block: &pbv1.ContentBlockStart_Provider{Provider: &pbv1.ProviderBlock{Kind: "x"}}},
		}}
	}
	startTool := func(idx int32) *pbv1.StreamEvent {
		return &pbv1.StreamEvent{Event: &pbv1.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pbv1.ContentBlockStart{Index: idx, Block: &pbv1.ContentBlockStart_ToolCall{ToolCall: &pbv1.ToolCallRef{Id: "c", Name: "n"}}},
		}}
	}
	stop := func(idx int32) *pbv1.StreamEvent {
		return &pbv1.StreamEvent{Event: &pbv1.StreamEvent_ContentBlockStop{
			ContentBlockStop: &pbv1.ContentBlockStop{Index: idx},
		}}
	}
	toolDelta := func(idx int32, frag string) *pbv1.StreamEvent {
		return &pbv1.StreamEvent{Event: &pbv1.StreamEvent_ToolCallDelta{
			ToolCallDelta: &pbv1.ToolCallDelta{Index: idx, ArgumentsDelta: frag},
		}}
	}
	msgStop := func() *pbv1.StreamEvent { return messageStopEvent("stop") }
	usage := func() *pbv1.StreamEvent { return usageEvent() }
	errEv := func() *pbv1.StreamEvent { return streamErrorEvent() }

	cases := []struct {
		name string
		seq  []*pbv1.StreamEvent
	}{
		{"bare text run", seq(text("a"), text("b"))},
		{"signed text block", seq(startText(0), text("a"), sig("s"), stop(0))},
		{"empty signed text block", seq(startText(0), sig("s"), stop(0))},
		{"text block then trailing sig", seq(startText(0), text("a"), stop(0), sig("s"))},
		{"thinking block", seq(startThinking(0), thinkingDelta("r"), stop(0))},
		{"tool block", seq(startTool(0), toolDelta(0, `{}`), stop(0))},
		{"parallel tool blocks", seq(startTool(0), startTool(1), toolDelta(0, `{"a":`), toolDelta(1, `{"b":`), stop(0), toolDelta(1, `1}`), stop(1))},
		{"message with usage", seq(startText(0), text("a"), stop(0), msgStop(), usage())},
		{"error is terminal", seq(text("a"), errEv(), usage())},
		{"implicit span then tool", seq(text("a"), startTool(0), toolDelta(0, `{}`), stop(0))},

		{"duplicate start at open index", seq(startText(0), text("a"), startText(0))},
		{"start while non-tool open", seq(startText(0), startThinking(1))},
		{"tool start while non-tool open", seq(startText(0), startTool(1))},
		{"non-tool start while tool open", seq(startTool(0), startText(1))},
		{"stop names no open block", seq(text("a"), stop(9))},
		{"stop mismatched index", seq(startText(0), text("a"), stop(3))},
		{"index reused after close", seq(startText(0), text("a"), stop(0), startText(0))},
		{"text delta inside thinking block", seq(startThinking(0), text("a"))},
		{"thinking delta inside text block", seq(startText(0), thinkingDelta("r"))},
		{"text delta while tool open", seq(startTool(0), text("a"))},
		{"tool delta names no open tool", seq(toolDelta(2, `{}`))},
		{"leading signature unbound", seq(sig("s"))},
		{"signature inside tool block", seq(startTool(0), toolDelta(0, `{}`), sig("s"), stop(0))},
		{"signature inside provider block", seq(startProvider(0), sig("s"), stop(0))},
		{"message stop while block open", seq(startText(0), text("a"), msgStop())},
		{"content after message stop", seq(startText(0), text("a"), stop(0), msgStop(), text("late"))},
		{"event after stream error", seq(text("a"), errEv(), text("late"))},
		{"ends with open block", seq(startText(0), text("a"))},
		{"ends with open tool", seq(startTool(0), toolDelta(0, `{}`))},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &streamDisciplineWalker{}
			var walkErr error
			var walkPos int
			for i, ev := range tc.seq {
				if walkErr == nil {
					if err := w.walk(ev); err != nil {
						walkErr = err
						walkPos = i
					}
				}
			}
			// The end-of-stream (missing stop) rule lives in walker.end(),
			// validateAcceptedStream's whole-slice form, so the mirror
			// compares walk()+end() against it.
			if walkErr == nil {
				walkErr = w.end()
			}
			validateErr := validateAcceptedStream(tc.seq)

			if (walkErr == nil) != (validateErr == nil) {
				t.Fatalf("walk mismatch: walker=%v validate=%v at position %d", walkErr, validateErr, walkPos)
			}
			if walkErr == nil {
				return
			}
			if got, want := unwrapAccepted(walkErr), unwrapAccepted(validateErr); got != want {
				t.Fatalf("error text mismatch: walker=%q validate=%q", got, want)
			}
			// The walker must have flagged the violation at the same position
			// validateAcceptedStream's slice index reports it (rules whose
			// message carries no position — e.g. an unbound signature — are
			// skipped).
			if !strings.Contains(unwrapAccepted(validateErr), "position ") {
				return
			}
			if !strings.Contains(unwrapAccepted(validateErr), "position "+strconv.Itoa(walkPos)) {
				t.Fatalf("walker flagged position %d but validateAcceptedStream says %q", walkPos, unwrapAccepted(validateErr))
			}
		})
	}
}

// --- pure state tests ------------------------------------------------------

// enforcePipeline loads a real pipeline so pluginStreamState carries a real
// loadedPlugin (manifest name, failure_mode, grant state). requireWASM keeps
// the suite honest in CI: these are host-mechanics tests, so any fixture with
// run_on_stream_chunk works.
func enforcePipeline(t *testing.T, name string) *PluginPipeline {
	t.Helper()
	requireWASM(t, fixturesDir+"/"+name+"/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{name})
	return pp
}

func enforceState(t *testing.T, name string) (*streamVerifierState, *pluginStreamState) {
	t.Helper()
	pp := enforcePipeline(t, name)
	vs := newStreamVerifierState(pp)
	if !vs.enforces() {
		t.Fatalf("fixture %s has no run_on_stream_chunk hook", name)
	}
	return vs, vs.plugins[0]
}

// TestEnforcePreCommitPassReplaysAcceptedEvent: a plugin whose emitted event
// violates the returned-side discipline under failure_mode "pass" has the
// event dropped and the accepted event replayed for that position — nothing
// terminates and the stream continues with a valid event in place.
func TestEnforcePreCommitPassReplaysAcceptedEvent(t *testing.T) {
	vs, pvs := enforceState(t, "test-stream-mutator")
	pvs.lp.failureMode = "pass"

	// The plugin's output stream already has index 0 open; it emits a second
	// start at index 0 — a duplicate-start discipline violation.
	accepted := textDelta("hello")
	emitted := &pbv1.StreamEvent{Event: &pbv1.StreamEvent_ContentBlockStart{
		ContentBlockStart: &pbv1.ContentBlockStart{Index: 0, Block: &pbv1.ContentBlockStart_Text{Text: &pbv1.TextBlock{}}},
	}}
	// Open index 0 on the returned walker first.
	if err := pvs.walker.walk(&pbv1.StreamEvent{Event: &pbv1.StreamEvent_ContentBlockStart{
		ContentBlockStart: &pbv1.ContentBlockStart{Index: 0, Block: &pbv1.ContentBlockStart_Text{Text: &pbv1.TextBlock{}}},
	}}); err != nil {
		t.Fatalf("setup walk: %v", err)
	}

	got, term := vs.acceptPluginOutput(pvs, accepted, emitted)
	if term != nil {
		t.Fatalf("pass mode must not terminate: %v", term)
	}
	if got != accepted {
		t.Fatalf("pass mode must replay the accepted event for this position, got %v", got)
	}
	if vs.terminal != nil {
		t.Fatalf("pass mode set the terminal flag: %v", vs.terminal)
	}
	// The replayed accepted event was fed to the walker (a bare delta is a
	// valid implicit-span event inside the open block).
	if err := pvs.walker.walk(textDelta("more")); err != nil {
		t.Fatalf("walker state diverged after replay: %v", err)
	}
}

// TestEnforcePreCommitBlockTerminates: the same violation under failure_mode
// "block" terminates with a typed terminal error attributed to the plugin.
func TestEnforcePreCommitBlockTerminates(t *testing.T) {
	vs, pvs := enforceState(t, "test-stream-mutator")
	pvs.lp.failureMode = "block"

	accepted := textDelta("hello")
	emitted := &pbv1.StreamEvent{Event: &pbv1.StreamEvent_ContentBlockStart{
		ContentBlockStart: &pbv1.ContentBlockStart{Index: 0, Block: &pbv1.ContentBlockStart_Text{Text: &pbv1.TextBlock{}}},
	}}
	// Open index 0 on the returned walker first.
	if err := pvs.walker.walk(&pbv1.StreamEvent{Event: &pbv1.StreamEvent_ContentBlockStart{
		ContentBlockStart: &pbv1.ContentBlockStart{Index: 0, Block: &pbv1.ContentBlockStart_Text{Text: &pbv1.TextBlock{}}},
	}}); err != nil {
		t.Fatalf("setup walk: %v", err)
	}

	_, term := vs.acceptPluginOutput(pvs, accepted, emitted)
	if term == nil {
		t.Fatal("block mode must terminate on a pre-commit violation")
	}
	if term.Kind != streamTerminalPlugin || term.Plugin != "test-stream-mutator" {
		t.Fatalf("attribution wrong: %+v", term)
	}
	if term.Index != 0 {
		t.Fatalf("expected index 0 attributed, got %d", term.Index)
	}
	if !strings.Contains(term.Error(), "reused") && !strings.Contains(term.Error(), "content block start") {
		t.Fatalf("invariant missing from terminal error: %v", term)
	}
	if vs.terminal != term {
		t.Fatal("terminal flag not recorded on the state")
	}
}

// TestEnforcePreCommitUnknownStopBlockTerminates pins a second per-event
// rule (stop naming no open block) through the same pre-commit path.
func TestEnforcePreCommitUnknownStopBlockTerminates(t *testing.T) {
	vs, pvs := enforceState(t, "test-stream-mutator")
	pvs.lp.failureMode = "block"

	accepted := textDelta("hello")
	emitted := stopEvent(7) // no open block at 7
	_, term := vs.acceptPluginOutput(pvs, accepted, emitted)
	if term == nil {
		t.Fatal("block mode must terminate on a stop naming no open block")
	}
	if term.Index != 7 {
		t.Fatalf("expected index 7 attributed, got %d", term.Index)
	}
	if !strings.Contains(term.Error(), "names no open block") {
		t.Fatalf("invariant missing from terminal error: %v", term)
	}
}

func stopEvent(idx int32) *pbv1.StreamEvent {
	return &pbv1.StreamEvent{Event: &pbv1.StreamEvent_ContentBlockStop{
		ContentBlockStop: &pbv1.ContentBlockStop{Index: idx},
	}}
}

// signedToolBlock builds the PB events of one signed tool block.
func signedToolBlock(index int32, id, name, signature, args string) []*pbv1.StreamEvent {
	return toolBlockDeltas(index, id, name, signature, args)
}

// TestEnforceScopeCloseLateTerminatesBothModes: a signed tool block whose
// args were rewritten while the token was kept is discovered only at the
// block's scope close — after the block's earlier events have been forwarded
// — so it terminates under BOTH failure modes.
func TestEnforceScopeCloseLateTerminatesBothModes(t *testing.T) {
	for _, mode := range []string{"pass", "block"} {
		t.Run(mode, func(t *testing.T) {
			vs, pvs := enforceState(t, "test-stream-mutator")
			pvs.lp.failureMode = mode

			accepted := signedToolBlock(0, "c", "search", streamSigA, `{"q":"original"}`)
			// The plugin's returned stream keeps the token over rewritten
			// arguments.
			returned := signedToolBlock(0, "c", "search", streamSigA, `{"q":"rewritten"}`)
			pvs.accepted = append(pvs.accepted, accepted...)
			pvs.returned = append(pvs.returned, returned...)

			err := vs.closeScope(pvs)
			if err == nil {
				t.Fatalf("stale signature must terminate under failure_mode %s", mode)
			}
			var term *StreamTerminalError
			if !errors.As(err, &term) {
				t.Fatalf("expected a typed terminal error under %s, got %T: %v", mode, err, err)
			}
			if term.Kind != streamTerminalPlugin || term.Plugin != "test-stream-mutator" {
				t.Fatalf("attribution wrong under %s: %+v", mode, term)
			}
			if term.Index != 0 {
				t.Fatalf("expected block 0 attributed under %s, got %d", mode, term.Index)
			}
			if !strings.Contains(term.Error(), "stale") {
				t.Fatalf("invariant missing under %s: %v", mode, term)
			}
			if term.Scope != 1 {
				t.Fatalf("expected scope ordinal 1, got %d", term.Scope)
			}
			if vs.terminal != term {
				t.Fatal("terminal flag not recorded")
			}
		})
	}
}

// TestEnforceScopeCloseValidPasses: a pass-through signed block (and a
// cleared-token rewrite) must NOT fire at scope close.
func TestEnforceScopeCloseValidPasses(t *testing.T) {
	t.Run("pass-through", func(t *testing.T) {
		vs, pvs := enforceState(t, "test-stream-mutator")
		accepted := signedToolBlock(0, "c", "search", streamSigA, `{"q":"x"}`)
		pvs.accepted = append(pvs.accepted, accepted...)
		pvs.returned = append(pvs.returned, accepted...)
		if err := vs.closeScope(pvs); err != nil {
			t.Fatalf("pass-through signed block fired: %v", err)
		}
	})
	t.Run("cleared token over rewritten content", func(t *testing.T) {
		vs, pvs := enforceState(t, "test-stream-mutator")
		accepted := signedToolBlock(0, "c", "search", streamSigA, `{"q":"x"}`)
		returned := signedToolBlock(0, "c", "search", "", `{"q":"y"}`) // token cleared
		pvs.accepted = append(pvs.accepted, accepted...)
		pvs.returned = append(pvs.returned, returned...)
		if err := vs.closeScope(pvs); err != nil {
			t.Fatalf("cleared token over rewritten content fired: %v", err)
		}
	})
	t.Run("unsigned rewrite", func(t *testing.T) {
		vs, pvs := enforceState(t, "test-stream-mutator")
		accepted := signedToolBlock(0, "c", "search", "", `{"q":"x"}`)
		returned := signedToolBlock(0, "c", "search", "", `{"q":"y"}`)
		pvs.accepted = append(pvs.accepted, accepted...)
		pvs.returned = append(pvs.returned, returned...)
		if err := vs.closeScope(pvs); err != nil {
			t.Fatalf("unsigned rewrite fired: %v", err)
		}
	})
}

// TestEnforceHostDefectIsHostTerminal: a malformed ACCEPTED stream is a host
// defect, never a plugin verdict. Even under a pass-mode plugin the stream
// terminates, and the terminal is attributed to "host".
func TestEnforceHostDefectIsHostTerminal(t *testing.T) {
	vs, _ := enforceState(t, "test-stream-mutator") // failure_mode pass

	if err := vs.host.walk(signatureDelta("floating-token")); err == nil {
		t.Fatal("an unbound signature_delta must be rejected by the host walker")
	} else {
		term := vs.terminate(streamTerminalHost, "host", -1, 0, err)
		if term.Kind != streamTerminalHost || term.Plugin != "host" {
			t.Fatalf("host defect attributed as a plugin verdict: %+v", term)
		}
		if !strings.Contains(term.Error(), "accepted stream") && !strings.Contains(err.Error(), "no covered content") {
			t.Fatalf("host defect invariant missing: %v", term)
		}
	}
}

// TestEnforceHostDefectNeverAppliesFailureMode: the end-to-end decision — a
// host defect terminates even though the only plugin is pass-mode, and the
// returned error is the typed host terminal.
func TestEnforceHostDefectNeverAppliesFailureMode(t *testing.T) {
	pp := enforcePipeline(t, "test-stream-mutator") // failure_mode pass
	sig := "floating"
	_, err := pp.RunOnStreamChunkVerified(context.Background(), 9001, &engine.StreamEvent{SignatureDelta: &sig})
	if err == nil {
		t.Fatal("host defect must terminate the stream")
	}
	var term *StreamTerminalError
	if !errors.As(err, &term) {
		t.Fatalf("expected typed terminal, got %T: %v", err, err)
	}
	if term.Kind != streamTerminalHost || term.Plugin != "host" {
		t.Fatalf("host defect attributed as a plugin verdict: %+v", term)
	}
	// The pass-mode plugin must not have replayed anything: the call returned
	// no events at all.
	pp.EndRequest(9001)
}

// TestEnforceNoDispatchAfterTerminal: once the state is terminal, every
// subsequent call returns the terminal error without dispatching to any
// plugin.
func TestEnforceNoDispatchAfterTerminal(t *testing.T) {
	vs, pvs := enforceState(t, "test-stream-mutator")
	pvs.lp.failureMode = "block"

	accepted := textDelta("hello")
	emitted := stopEvent(3)
	_, term := vs.acceptPluginOutput(pvs, accepted, emitted)
	if term == nil {
		t.Fatal("setup: expected a pre-commit terminal")
	}
	if vs.terminal == nil {
		t.Fatal("terminal flag not set")
	}
	// Further calls short-circuit with the same terminal.
	_, again := vs.acceptPluginOutput(pvs, textDelta("x"), textDelta("y"))
	if again != term {
		t.Fatalf("expected the recorded terminal, got %v", again)
	}
}

// TestEnforceEndStreamClosesFinalScope: a stream that never closes a scope
// (bare implicit-span events) is still verified at end-of-stream; a dropped
// signature over unchanged content is a violation found only there.
func TestEnforceEndStreamClosesFinalScope(t *testing.T) {
	vs, pvs := enforceState(t, "test-stream-mutator")

	// Accepted: implicit span + current-block signature. Returned: the
	// content survived but the token was dropped. No scope ever closed, so
	// only the end-of-stream scope close can catch it.
	pvs.accepted = append(pvs.accepted, textDelta("the quick brown fox"), signatureDelta(streamSigA))
	pvs.returned = append(pvs.returned, textDelta("the quick brown fox"))

	err := vs.closeScope(pvs) // end-of-stream scope close
	if err == nil {
		t.Fatal("dropped signature must be caught at the end-of-stream scope close")
	}
	var term *StreamTerminalError
	if !errors.As(err, &term) {
		t.Fatalf("expected typed terminal, got %T", err)
	}
	if term.Kind != streamTerminalPlugin {
		t.Fatalf("dropped signature is a plugin violation, got %+v", term)
	}
	if !strings.Contains(term.Error(), "dropped") {
		t.Fatalf("expected a dropped classification, got %v", term)
	}
}

// TestEnforceEndStreamHostMissingStop: an accepted stream ending with an
// open content block is a host defect found at end-of-stream.
func TestEnforceEndStreamHostMissingStop(t *testing.T) {
	vs, _ := enforceState(t, "test-stream-mutator")

	if err := vs.host.walk(&pbv1.StreamEvent{Event: &pbv1.StreamEvent_ContentBlockStart{
		ContentBlockStart: &pbv1.ContentBlockStart{Index: 0, Block: &pbv1.ContentBlockStart_Text{Text: &pbv1.TextBlock{}}},
	}}); err != nil {
		t.Fatalf("setup walk: %v", err)
	}
	err := vs.host.end()
	if err == nil {
		t.Fatal("accepted stream ending with an open block must be a host defect")
	}
	term := vs.terminate(streamTerminalHost, "host", -1, 0, err)
	if term.Kind != streamTerminalHost {
		t.Fatalf("missing stop must be a host defect, got %+v", term)
	}
}

// TestVerifyStreamPolicyConsumesSDKRegistry pins the enforcement boundary
// independently of WASM dispatch. The registry is authoritative: semantic
// changes need their section, cardinality/action changes additionally need
// topology, and a host fact is never grantable.
func TestVerifyStreamPolicyConsumesSDKRegistry(t *testing.T) {
	text := func(s string) *pbv1.StreamEvent { return textDelta(s) }
	usage := func(n int32) *pbv1.StreamEvent {
		return &pbv1.StreamEvent{Event: &pbv1.StreamEvent_Usage{Usage: &pbv1.Usage{InputTokens: n}}}
	}
	for _, tc := range []struct {
		name     string
		accepted []*pbv1.StreamEvent
		returned []*pbv1.StreamEvent
		grants   []string
		want     string
	}{
		{
			name:     "one-for-one text rewrite needs assistant",
			accepted: []*pbv1.StreamEvent{text("secret")},
			returned: []*pbv1.StreamEvent{text("redacted")},
			want:     "ir.messages.write.assistant",
		},
		{
			name:     "granted one-for-one text rewrite passes",
			accepted: []*pbv1.StreamEvent{text("secret")},
			returned: []*pbv1.StreamEvent{text("redacted")},
			grants:   []string{"ir.messages.write.assistant"},
		},
		{
			name:     "suppression needs topology and semantic grant",
			accepted: []*pbv1.StreamEvent{text("secret")},
			returned: nil,
			grants:   []string{"ir.stream.write"},
			want:     "ir.messages.write.assistant",
		},
		{
			name:     "suppression passes with union of grants",
			accepted: []*pbv1.StreamEvent{text("secret")},
			returned: nil,
			grants:   []string{"ir.stream.write", "ir.messages.write.assistant"},
		},
		{
			name:     "usage is host owned even with every grant",
			accepted: []*pbv1.StreamEvent{usage(1)},
			returned: []*pbv1.StreamEvent{usage(2)},
			grants:   []string{"ir.stream.write", "ir.messages.write.assistant"},
			want:     "host-owned",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyStreamPolicy(tc.accepted, tc.returned, grant(tc.grants...))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("policy rejected granted mutation: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestVerifyStreamPolicyRecursesAddedMessageFields proves that a topology
// grant authorizes the new boundary itself, never the assistant-owned facts
// nested below it. A ToolCallRef's id/name must be charged on both invention
// and removal; a bound signature remains the signature verifier's concern.
func TestVerifyStreamPolicyRecursesAddedMessageFields(t *testing.T) {
	toolStart := func(signature string) *pbv1.StreamEvent {
		return &pbv1.StreamEvent{Event: &pbv1.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pbv1.ContentBlockStart{
				Index: 0,
				Block: &pbv1.ContentBlockStart_ToolCall{ToolCall: &pbv1.ToolCallRef{
					Id: "call_1", Name: "read", Signature: signature,
				}},
			},
		}}
	}
	for _, tc := range []struct {
		name     string
		accepted []*pbv1.StreamEvent
		returned []*pbv1.StreamEvent
	}{
		{"unsigned invention", nil, []*pbv1.StreamEvent{toolStart("")}},
		{"unsigned removal", []*pbv1.StreamEvent{toolStart("")}, nil},
		{"signed invention", nil, []*pbv1.StreamEvent{toolStart("provider-token")}},
		{"signed removal", []*pbv1.StreamEvent{toolStart("provider-token")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyStreamPolicy(tc.accepted, tc.returned, grant("ir.stream.write"))
			if err == nil || !strings.Contains(err.Error(), "ir.messages.write.assistant") {
				t.Fatalf("topology alone authorized nested tool semantics: %v", err)
			}
			err = verifyStreamPolicy(tc.accepted, tc.returned, grant("ir.stream.write", "ir.messages.write.assistant"))
			if err != nil {
				t.Fatalf("union grants rejected nested tool transaction: %v", err)
			}
		})
	}
}

// TestVerifyStreamPolicyCorrelatesExactEventsFirst pins occurrence-aware
// correlation. Exact host-owned events must reserve before changed fragments
// are paired, and an exact reordering is topology-only rather than a made-up
// semantic rewrite. The cases deliberately include duplicates.
func TestVerifyStreamPolicyCorrelatesExactEventsFirst(t *testing.T) {
	text := func(s string) *pbv1.StreamEvent { return textDelta(s) }
	messageStart := &pbv1.StreamEvent{Event: &pbv1.StreamEvent_MessageStart{
		MessageStart: &pbv1.MessageStart{Role: "assistant", Id: "m", Model: "model"},
	}}
	streamErr := &pbv1.StreamEvent{Event: &pbv1.StreamEvent_Error{
		Error: &pbv1.StreamError{Code: 503, Message: "unchanged"},
	}}
	trailingHostEvents := map[string]*pbv1.StreamEvent{
		"usage":         usageEvent(),
		"message start": messageStart,
		"stream error":  streamErr,
		"message stop":  messageStopEvent("stop"),
	}
	for name, trailing := range trailingHostEvents {
		t.Run("suppression before unchanged "+name, func(t *testing.T) {
			err := verifyStreamPolicy(
				[]*pbv1.StreamEvent{text("remove"), trailing},
				[]*pbv1.StreamEvent{trailing},
				grant("ir.stream.write", "ir.messages.write.assistant"),
			)
			if err != nil {
				t.Fatalf("shifted unchanged host event was misclassified: %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name     string
		accepted []*pbv1.StreamEvent
		returned []*pbv1.StreamEvent
		grants   []string
		want     string
	}{
		{
			name:     "assembly before unchanged usage",
			accepted: []*pbv1.StreamEvent{text("a"), text("b"), usageEvent()},
			returned: []*pbv1.StreamEvent{text("ab"), usageEvent()},
			grants:   []string{"ir.stream.write", "ir.messages.write.assistant"},
		},
		{
			name:     "assembly before unchanged message stop",
			accepted: []*pbv1.StreamEvent{text("a"), text("b"), messageStopEvent("stop")},
			returned: []*pbv1.StreamEvent{text("ab"), messageStopEvent("stop")},
			grants:   []string{"ir.stream.write", "ir.messages.write.assistant"},
		},
		{
			name:     "pure exact reorder is topology only",
			accepted: []*pbv1.StreamEvent{text("a"), text("b")},
			returned: []*pbv1.StreamEvent{text("b"), text("a")},
			grants:   []string{"ir.stream.write"},
		},
		{
			name:     "reorder plus mutation needs semantic union",
			accepted: []*pbv1.StreamEvent{text("a"), text("b")},
			returned: []*pbv1.StreamEvent{text("b"), text("c")},
			grants:   []string{"ir.stream.write"},
			want:     "ir.messages.write.assistant",
		},
		{
			name:     "duplicate cardinality still charges removed semantic event",
			accepted: []*pbv1.StreamEvent{text("a"), text("a")},
			returned: []*pbv1.StreamEvent{text("a")},
			grants:   []string{"ir.stream.write"},
			want:     "ir.messages.write.assistant",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyStreamPolicy(tc.accepted, tc.returned, grant(tc.grants...))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("exact-first transaction rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestEnforcePassOutputIsAtomicRegression reproduces the pre-round-2 state
// poison: an invalid start marked index 1 seen before pass-mode replay, so a
// later legitimate start at that index was rejected. Both the walker and the
// fan-out candidate now commit only after complete validation succeeds.
func TestEnforcePassOutputIsAtomicRegression(t *testing.T) {
	vs, pvs := enforceState(t, "test-stream-mutator")
	startText := func(idx int32) *pbv1.StreamEvent {
		return &pbv1.StreamEvent{Event: &pbv1.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pbv1.ContentBlockStart{Index: idx, Block: &pbv1.ContentBlockStart_Text{Text: &pbv1.TextBlock{}}},
		}}
	}
	startThinking := func(idx int32) *pbv1.StreamEvent {
		return &pbv1.StreamEvent{Event: &pbv1.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pbv1.ContentBlockStart{Index: idx, Block: &pbv1.ContentBlockStart_Thinking{Thinking: &pbv1.ThinkingBlock{}}},
		}}
	}
	if _, term := vs.acceptPluginOutput(pvs, startText(0), startText(0)); term != nil {
		t.Fatalf("setup start: %v", term)
	}

	// The first candidate child is valid, the second overlaps the text block.
	// Pass mode must discard BOTH children and replay the accepted delta once.
	got, term := vs.acceptPluginOutputs(pvs, textDelta("original"), []*pbv1.StreamEvent{textDelta("replacement"), startThinking(1)})
	if term != nil {
		t.Fatalf("pass-mode candidate terminated: %v", term)
	}
	if len(got) != 1 || got[0].GetTextDelta() != "original" {
		t.Fatalf("pass replay = %+v, want exactly the accepted event", got)
	}
	if _, term := vs.acceptPluginOutput(pvs, stopEvent(0), stopEvent(0)); term != nil {
		t.Fatalf("close original block: %v", term)
	}
	if _, term := vs.acceptPluginOutput(pvs, startThinking(1), startThinking(1)); term != nil {
		t.Fatalf("legitimate index after rejected candidate was poisoned: %v", term)
	}
}

// TestEnforceParallelToolPrefixAllowsSiblingOpen pins the prefix/final split:
// closing tool 0 while tool 1 remains live is a valid scope prefix. Completeness
// is enforced only when the returned stream reaches MessageStop/end-of-stream.
func TestEnforceParallelToolPrefixAllowsSiblingOpen(t *testing.T) {
	vs, pvs := enforceState(t, "test-stream-mutator")
	startTool := func(idx int32, id, name string) *pbv1.StreamEvent {
		return &pbv1.StreamEvent{Event: &pbv1.StreamEvent_ContentBlockStart{
			ContentBlockStart: &pbv1.ContentBlockStart{Index: idx, Block: &pbv1.ContentBlockStart_ToolCall{ToolCall: &pbv1.ToolCallRef{Id: id, Name: name}}},
		}}
	}
	arg := func(idx int32, fragment string) *pbv1.StreamEvent {
		return &pbv1.StreamEvent{Event: &pbv1.StreamEvent_ToolCallDelta{ToolCallDelta: &pbv1.ToolCallDelta{Index: idx, ArgumentsDelta: fragment}}}
	}
	accepted := []*pbv1.StreamEvent{
		startTool(0, "a", "one"), startTool(1, "b", "two"),
		arg(0, `{`), arg(1, `{`), stopEvent(0),
	}
	pvs.accepted = append(pvs.accepted, accepted...)
	pvs.returned = append(pvs.returned, accepted...)
	if err := vs.closeScope(pvs); err != nil {
		t.Fatalf("valid parallel-tool prefix rejected: %v", err)
	}
	pvs.accepted = append(pvs.accepted, arg(1, `}`), stopEvent(1))
	pvs.returned = append(pvs.returned, arg(1, `}`), stopEvent(1))
	if err := vs.closeScope(pvs); err != nil {
		t.Fatalf("parallel tool completion rejected: %v", err)
	}
}

// TestEnforceScopeOrdinalAdvancesOnceAcrossPlugins pins the attribution
// contract: a completed accepted scope has one stable ordinal even when every
// plugin gets a chance to inspect it. The former per-plugin increment made the
// same host block report a different scope depending on pipeline order.
func TestEnforceScopeOrdinalAdvancesOnceAcrossPlugins(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-stream-mutator/plugin.wasm")
	requireWASM(t, fixturesDir+"/test-tool-rewriter/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-stream-mutator", "test-tool-rewriter"})
	const reqID = 7019

	runVerified(t, pp, reqID, engine.StreamEvent{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}})
	runVerified(t, pp, reqID, engine.StreamEvent{TextDelta: strPtr("ordinary text")})
	runVerified(t, pp, reqID, engine.StreamEvent{BlockStop: &engine.BlockStop{Index: 0}})

	pp.mu.Lock()
	vs := pp.streamVerify[reqID]
	pluginScopes := []int{vs.plugins[0].scopeNum, vs.plugins[1].scopeNum}
	pp.mu.Unlock()
	for i, got := range pluginScopes {
		if got != 1 {
			t.Fatalf("plugin %d saw one accepted block close but reported scope %d, want 1", i, got)
		}
	}
	pp.EndRequest(reqID)
}

// TestEnforceTransformedBoundariesUseDownstreamScope exercises the exact
// transformed-stream case: one non-close host text delta fans out into TWO
// completed boundaries before a downstream plugin sees it. The downstream
// no-grant reindex is a late policy terminal at scope 2, never scope 0.
func TestEnforceTransformedBoundariesUseDownstreamScope(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-stream-fanout-boundaries/plugin.wasm")
	requireWASM(t, fixturesDir+"/test-stream-reindex-nogrant/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-stream-fanout-boundaries", "test-stream-reindex-nogrant"})
	const reqID = 7021

	_, err := pp.RunOnStreamChunkVerified(context.Background(), reqID, &engine.StreamEvent{TextDelta: strPtr("source")})
	var term *StreamTerminalError
	if !errors.As(err, &term) || term.Kind != streamTerminalPlugin || term.Plugin != "test-stream-reindex-nogrant" {
		t.Fatalf("expected downstream topology terminal, got %T: %v", err, err)
	}
	if term.Scope != 2 {
		t.Fatalf("two downstream boundaries reported scope %d, want 2: %v", term.Scope, term)
	}
	pp.EndRequest(reqID)
}

// TestEnforceScopeWatermarksConvergeAcrossCalls pins the two timing variants
// a topology writer may legally produce: suppress an accepted stop then emit
// it later, or emit it early then suppress the later accepted stop. The
// terminal after either shape must use the converged logical scope, never the
// sum of each call's local max.
func TestEnforceScopeWatermarksConvergeAcrossCalls(t *testing.T) {
	usage := func(tokens int32) *pbv1.StreamEvent {
		return &pbv1.StreamEvent{Event: &pbv1.StreamEvent_Usage{Usage: &pbv1.Usage{InputTokens: tokens}}}
	}
	for _, tc := range []struct {
		name  string
		calls [][2]int // accepted closes, returned closes for successive calls
		want  int
	}{
		{
			name:  "accepted stop suppressed then returned later",
			calls: [][2]int{{1, 0}, {0, 1}},
			want:  1,
		},
		{
			name:  "returned stop early then accepted later",
			calls: [][2]int{{0, 1}, {1, 0}},
			want:  1,
		},
		{
			name:  "two returned scopes then one accepted close",
			calls: [][2]int{{0, 2}, {1, 0}},
			want:  2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vs, pvs := enforceState(t, "test-stream-mutator")
			for _, call := range tc.calls {
				if got := pvs.recordScopeCloses(call[0], call[1]); got != max(pvs.acceptedCloseCount, pvs.returnedCloseCount) {
					t.Fatalf("watermark scope = %d, want max(%d, %d)", got, pvs.acceptedCloseCount, pvs.returnedCloseCount)
				}
			}
			if pvs.scopeNum != tc.want {
				t.Fatalf("converged scope = %d, want %d", pvs.scopeNum, tc.want)
			}

			// A real policy violation after the timing shape must preserve the
			// converged ordinal in its operator-visible terminal.
			pvs.accepted = []*pbv1.StreamEvent{usage(1)}
			pvs.returned = []*pbv1.StreamEvent{usage(2)}
			err := vs.checkScope(pvs, pvs.scopeNum)
			var term *StreamTerminalError
			if !errors.As(err, &term) || term.Kind != streamTerminalPlugin || term.Scope != tc.want {
				t.Fatalf("violation terminal = %T: %v, want scope %d", err, err, tc.want)
			}
		})
	}
}

// TestEnforceScopeWatermarksConvergeAcrossCallsProduction drives the two
// legal boundary moves through real WASM hooks. The per-plugin watermark must
// converge at scope 1 after the delayed/early counterpart arrives on the next
// host event; the old per-call max reported scope 2.
func TestEnforceScopeWatermarksConvergeAcrossCallsProduction(t *testing.T) {
	for _, tc := range []struct {
		name   string
		plugin string
	}{
		{"accepted stop delayed to usage", "test-stream-delay-stop"},
		{"returned stop emitted early", "test-stream-early-stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireWASM(t, fixturesDir+"/"+tc.plugin+"/plugin.wasm")
			pp := newTestPipeline(t, fixturesDir, []string{tc.plugin})
			const reqID = 7027
			seq := []engine.StreamEvent{
				{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}},
				{TextDelta: strPtr("text")},
				{BlockStop: &engine.BlockStop{Index: 0}},
				{Usage: &engine.StreamUsage{InputTokens: 1, OutputTokens: 1}},
			}
			for _, event := range seq {
				e := event
				if _, err := pp.RunOnStreamChunkVerified(context.Background(), reqID, &e); err != nil {
					t.Fatalf("RunOnStreamChunkVerified(%+v): %v", event, err)
				}
			}
			pp.mu.Lock()
			got := pp.streamVerify[reqID].plugins[0].scopeNum
			pp.mu.Unlock()
			if got != 1 {
				t.Fatalf("moved boundary converged at scope %d, want 1", got)
			}
			if err := pp.EndStreamVerified(reqID); err != nil {
				t.Fatalf("EndStreamVerified: %v", err)
			}
			pp.EndRequest(reqID)
		})
	}
}

// TestEndStreamVerifiedUsesSeparateTrailingScope pins the defined terminal
// ordinal for a boundary-less transaction after completed scopes. It is one
// final scope for diagnostics, but does not advance either close watermark.
func TestEndStreamVerifiedUsesSeparateTrailingScope(t *testing.T) {
	pp := enforcePipeline(t, "test-stream-mutator")
	vs := newStreamVerifierState(pp)
	const reqID = 7026
	pp.mu.Lock()
	pp.streamVerify[reqID] = vs
	pp.mu.Unlock()
	pvs := vs.plugins[0]
	pvs.recordScopeCloses(1, 1)
	accepted := &pbv1.StreamEvent{Event: &pbv1.StreamEvent_Usage{Usage: &pbv1.Usage{InputTokens: 1}}}
	returned := &pbv1.StreamEvent{Event: &pbv1.StreamEvent_Usage{Usage: &pbv1.Usage{InputTokens: 2}}}
	if err := vs.host.walk(accepted); err != nil {
		t.Fatalf("host setup: %v", err)
	}
	if err := pvs.walker.walk(returned); err != nil {
		t.Fatalf("returned setup: %v", err)
	}
	pvs.accepted = []*pbv1.StreamEvent{accepted}
	pvs.returned = []*pbv1.StreamEvent{returned}

	err := pp.EndStreamVerified(reqID)
	var term *StreamTerminalError
	if !errors.As(err, &term) || term.Kind != streamTerminalPlugin || term.Scope != 2 {
		t.Fatalf("trailing terminal = %T: %v, want scope 2", err, err)
	}
	if pvs.acceptedCloseCount != 1 || pvs.returnedCloseCount != 1 {
		t.Fatalf("end-of-stream mutated close watermarks: accepted=%d returned=%d", pvs.acceptedCloseCount, pvs.returnedCloseCount)
	}
	pp.EndRequest(reqID)
}

// TestEnforceReturnedCompleteScopeBeforeRelease proves a LAST plugin cannot
// invent and close a complete scope from a bare non-close input, then rely on
// EndStreamVerified running after the unauthorized events have reached the
// serializer. A returned-side close triggers the transaction before this call
// returns any output.
func TestEnforceReturnedCompleteScopeBeforeRelease(t *testing.T) {
	t.Run("ungranted complete text block is terminal with no output", func(t *testing.T) {
		requireWASM(t, fixturesDir+"/test-stream-complete-block-nogrant/plugin.wasm")
		pp := newTestPipeline(t, fixturesDir, []string{"test-stream-complete-block-nogrant"})
		const reqID = 7022
		out, err := pp.RunOnStreamChunkVerified(context.Background(), reqID, &engine.StreamEvent{TextDelta: strPtr("accepted")})
		if len(out) != 0 {
			t.Fatalf("ungranted completed scope escaped before terminal: %+v", out)
		}
		var term *StreamTerminalError
		if !errors.As(err, &term) || term.Kind != streamTerminalPlugin || term.Plugin != "test-stream-complete-block-nogrant" {
			t.Fatalf("expected no-grant plugin terminal, got %T: %v", err, err)
		}
		if term.Scope != 1 || !strings.Contains(term.Error(), "ir.stream.write") {
			t.Fatalf("wrong returned-scope terminal: %v", term)
		}
		pp.EndRequest(reqID)
	})

	t.Run("union grants allow complete text block", func(t *testing.T) {
		requireWASM(t, fixturesDir+"/test-stream-complete-block-granted/plugin.wasm")
		pp := newTestPipeline(t, fixturesDir, []string{"test-stream-complete-block-granted"})
		const reqID = 7023
		out, err := pp.RunOnStreamChunkVerified(context.Background(), reqID, &engine.StreamEvent{TextDelta: strPtr("accepted")})
		if err != nil {
			t.Fatalf("granted complete scope rejected: %v", err)
		}
		if len(out) != 3 || out[0].BlockStart == nil || out[1].TextDelta == nil || out[2].BlockStop == nil {
			t.Fatalf("granted complete scope lowered incorrectly: %+v", out)
		}
		if err := pp.EndStreamVerified(reqID); err != nil {
			t.Fatalf("granted completed scope failed at end: %v", err)
		}
		pp.EndRequest(reqID)
	})

	t.Run("invented signed tool is signature added even with grants", func(t *testing.T) {
		requireWASM(t, fixturesDir+"/test-stream-complete-signed-tool/plugin.wasm")
		pp := newTestPipeline(t, fixturesDir, []string{"test-stream-complete-signed-tool"})
		const reqID = 7024
		out, err := pp.RunOnStreamChunkVerified(context.Background(), reqID, &engine.StreamEvent{TextDelta: strPtr("accepted")})
		if len(out) != 0 {
			t.Fatalf("invented signed tool escaped before terminal: %+v", out)
		}
		var term *StreamTerminalError
		if !errors.As(err, &term) || term.Kind != streamTerminalPlugin || !strings.Contains(term.Error(), "signature added") {
			t.Fatalf("invented signature did not classify as added: %T: %v", err, err)
		}
		pp.EndRequest(reqID)
	})

	t.Run("two returned scopes are one atomic check at the last ordinal", func(t *testing.T) {
		requireWASM(t, fixturesDir+"/test-stream-complete-block-nogrant/plugin.wasm")
		pp := newTestPipeline(t, fixturesDir, []string{"test-stream-complete-block-nogrant"})
		const reqID = 7025
		out, err := pp.RunOnStreamChunkVerified(context.Background(), reqID, &engine.StreamEvent{TextDelta: strPtr("two")})
		if len(out) != 0 {
			t.Fatalf("partial two-scope output escaped before terminal: %+v", out)
		}
		var term *StreamTerminalError
		if !errors.As(err, &term) || term.Kind != streamTerminalPlugin || term.Scope != 2 {
			t.Fatalf("two returned scopes reported wrong terminal: %T: %v", err, err)
		}
		pp.EndRequest(reqID)
	})
}

// TestEndStreamVerifiedRejectsReturnedMissingStop closes the laundering hole
// where a plugin could forward a start/delta but suppress its stop and rely on
// ir.stream.write to make the incomplete returned block look like whole-block
// suppression. Returned completeness is mandatory even when there is no
// remaining policy scope to compare.
func TestEndStreamVerifiedRejectsReturnedMissingStop(t *testing.T) {
	pp := enforcePipeline(t, "test-stream-mutator")
	vs := newStreamVerifierState(pp)
	const reqID = 7020
	pp.mu.Lock()
	pp.streamVerify[reqID] = vs
	pp.mu.Unlock()
	pvs := vs.plugins[0]
	start := &pbv1.StreamEvent{Event: &pbv1.StreamEvent_ContentBlockStart{
		ContentBlockStart: &pbv1.ContentBlockStart{Index: 0, Block: &pbv1.ContentBlockStart_ToolCall{ToolCall: &pbv1.ToolCallRef{Id: "c", Name: "read"}}},
	}}
	stop := stopEvent(0)
	if err := vs.host.walk(start); err != nil {
		t.Fatalf("host start: %v", err)
	}
	if err := vs.host.walk(stop); err != nil {
		t.Fatalf("host stop: %v", err)
	}
	if err := pvs.walker.walk(start); err != nil {
		t.Fatalf("returned start: %v", err)
	}
	// Simulate a prior completed policy transaction so only walker.end can
	// catch the missing returned stop.
	pvs.accepted = []*pbv1.StreamEvent{start, stop}
	pvs.returned = []*pbv1.StreamEvent{start}
	pvs.scopeStart = len(pvs.accepted)

	err := pp.EndStreamVerified(reqID)
	var term *StreamTerminalError
	if !errors.As(err, &term) || term.Kind != streamTerminalPlugin {
		t.Fatalf("missing returned stop must be a plugin terminal, got %T: %v", err, err)
	}
	if !strings.Contains(term.Error(), "missing ContentBlockStop") {
		t.Fatalf("missing-stop invariant absent: %v", term)
	}
	pp.EndRequest(reqID)
}

// TestStreamTerminalErrorFormatting pins the operator-visible attribution
// text: kind, plugin, index and the invariant all appear.
func TestStreamTerminalErrorFormatting(t *testing.T) {
	term := &StreamTerminalError{
		Plugin: "test-tool-rewriter", Kind: streamTerminalPlugin, Index: 2, Scope: 3,
		Err: errors.New("tool block 2 signature stale"),
	}
	msg := term.Error()
	for _, want := range []string{"test-tool-rewriter", "block 2", "scope 3", "stale"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("terminal error %q missing %q", msg, want)
		}
	}
	if !errors.Is(term, term.Err) {
		t.Fatal("Unwrap must expose the underlying invariant error")
	}
}

// --- end-to-end through the verified entry point --------------------------

func runVerified(t *testing.T, pp *PluginPipeline, reqID uint64, ev engine.StreamEvent) []engine.StreamEvent {
	t.Helper()
	out, err := pp.RunOnStreamChunkVerified(context.Background(), reqID, &ev)
	if err != nil {
		t.Fatalf("RunOnStreamChunkVerified: %v", err)
	}
	return out
}

// engineSignedToolBlock builds the engine events of one signed tool block.
func engineSignedToolBlock(index int, id, name, signature, args string) []engine.StreamEvent {
	return []engine.StreamEvent{
		{ToolCallStart: &engine.ToolCallStart{Index: index, ID: id, Name: name, Signature: signature}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: index, ArgumentsDelta: args}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: index}},
	}
}

// TestStreamEnforcementStaleSignedBlockTerminates is the pinned regression:
// a plugin that rewrites a signed block's args and keeps the token terminates
// the stream under BOTH failure modes (the violation is late — found at the
// block's scope close, after its events were already forwarded).
//
// test-tool-rewriter rewrites ToolCallDelta and keeps ToolCallStart (and its
// signature) — exactly the stale shape.
func TestStreamEnforcementStaleSignedBlockTerminates(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-tool-rewriter/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-tool-rewriter"}) // failure_mode pass
	const reqID = 7001

	// Signed tool block: the start carries the provider token.
	out := runVerified(t, pp, reqID, engine.StreamEvent{ToolCallStart: &engine.ToolCallStart{
		Index: 0, ID: "call_1", Name: "search", Signature: streamSigA,
	}})
	if len(out) != 1 || out[0].ToolCallStart == nil {
		t.Fatalf("expected the signed start to pass through, got %+v", out)
	}
	runVerified(t, pp, reqID, engine.StreamEvent{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{"q":"original"}`}})

	// The block close is where the stale token is discovered — late, so the
	// plugin's failure_mode (pass) does NOT apply: the stream terminates.
	_, err := pp.RunOnStreamChunkVerified(context.Background(), reqID, &engine.StreamEvent{ToolCallEnd: &engine.ToolCallEnd{Index: 0}})
	if err == nil {
		t.Fatal("stale signature must terminate the stream")
	}
	var term *StreamTerminalError
	if !errors.As(err, &term) {
		t.Fatalf("expected typed terminal, got %T: %v", err, err)
	}
	if term.Kind != streamTerminalPlugin || term.Plugin != "test-tool-rewriter" {
		t.Fatalf("attribution wrong: %+v", term)
	}
	if term.Index != 0 {
		t.Fatalf("expected block 0, got %d", term.Index)
	}
	if !strings.Contains(term.Error(), "stale") {
		t.Fatalf("invariant missing: %v", term)
	}

	// No downstream dispatch after the terminal: the next event returns the
	// same terminal error without invoking any plugin.
	_, again := pp.RunOnStreamChunkVerified(context.Background(), reqID, &engine.StreamEvent{TextDelta: strPtr("later")})
	if !errors.As(again, &term) || term == nil {
		t.Fatalf("expected the recorded terminal on the next call, got %v", again)
	}
	pp.EndRequest(reqID)
}

// TestStreamEnforcementValidSignedStreamPasses: the enforcement must not fire
// on a valid signed stream — signed text block, signed tool block, message
// stop — through the exact production path, including the end-of-stream
// scope close.
func TestStreamEnforcementValidSignedStreamPasses(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-stream-mutator/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-stream-mutator"})
	const reqID = 7002

	seq := []engine.StreamEvent{
		{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}},
		{TextDelta: strPtr("the quick brown fox")},
		{SignatureDelta: strPtr(streamSigA)},
		{BlockStop: &engine.BlockStop{Index: 0}},
	}
	seq = append(seq, engineSignedToolBlock(1, "call_2", "read", streamSigB, `{"path":"x"}`)...)
	seq = append(seq, engine.StreamEvent{FinishReason: "tool_calls"})

	for _, ev := range seq {
		e := ev
		runVerified(t, pp, reqID, e)
	}
	if err := pp.EndStreamVerified(reqID); err != nil {
		t.Fatalf("valid signed stream fired at end-of-stream: %v", err)
	}
	pp.EndRequest(reqID)
}

// TestStreamEnforcementRejectsUngrantedTextMutation is the production-path
// regression for the review finding that prompted 2b round 2. The fixture
// deliberately declares only env.log yet rewrites a streamed assistant delta;
// the scope transaction must reject it rather than silently letting a plugin
// mutate content outside its declared capability.
func TestStreamEnforcementRejectsUngrantedTextMutation(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-stream-mutator-nogrant/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-stream-mutator-nogrant"})
	const reqID = 7010

	start := engine.StreamEvent{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}}
	if _, err := pp.RunOnStreamChunkVerified(context.Background(), reqID, &start); err != nil {
		t.Fatalf("block start: %v", err)
	}
	secret := "the secret plan"
	if _, err := pp.RunOnStreamChunkVerified(context.Background(), reqID, &engine.StreamEvent{TextDelta: &secret}); err != nil {
		t.Fatalf("delta before scope close: %v", err)
	}
	_, err := pp.RunOnStreamChunkVerified(context.Background(), reqID, &engine.StreamEvent{BlockStop: &engine.BlockStop{Index: 0}})
	if err == nil {
		t.Fatal("ungranted text rewrite must terminate at its completed scope")
	}
	var term *StreamTerminalError
	if !errors.As(err, &term) || term.Kind != streamTerminalPlugin || term.Plugin != "test-stream-mutator-nogrant" {
		t.Fatalf("wrong terminal attribution: %T: %v", err, err)
	}
	if !strings.Contains(term.Error(), "ir.messages.write.assistant") {
		t.Fatalf("missing required grant in terminal: %v", term)
	}
	pp.EndRequest(reqID)
}

// TestStreamEnforcementEndToEndHostDefect: an unbound signature_delta from
// the accepted side terminates the verified stream as a HOST defect — the
// plugin's failure_mode never applies (test-stream-mutator is pass-mode, and nothing
// was replayed).
func TestStreamEnforcementEndToEndHostDefect(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-stream-mutator/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-stream-mutator"})
	const reqID = 7003

	sig := "floating"
	out, err := pp.RunOnStreamChunkVerified(context.Background(), reqID, &engine.StreamEvent{SignatureDelta: &sig})
	if err == nil {
		t.Fatal("host defect must terminate")
	}
	if len(out) != 0 {
		t.Fatalf("host defect must not forward any event (no pass-mode replay), got %+v", out)
	}
	var term *StreamTerminalError
	if !errors.As(err, &term) || term.Kind != streamTerminalHost || term.Plugin != "host" {
		t.Fatalf("expected host terminal, got %T: %v", err, err)
	}
	pp.EndRequest(reqID)
}

// TestStreamEnforcementStateLifecycle: the enforcement state is created on
// the first verified call, survives across calls, and is dropped by
// EndRequest — the same lifecycle as the streamKinds tracker.
func TestStreamEnforcementStateLifecycle(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-stream-mutator/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-stream-mutator"})
	const reqID = 7004

	pp.mu.Lock()
	_, exists := pp.streamVerify[reqID]
	pp.mu.Unlock()
	if exists {
		t.Fatal("enforcement state must not exist before the first verified call")
	}

	runVerified(t, pp, reqID, engine.StreamEvent{TextDelta: strPtr("hello")})
	pp.mu.Lock()
	vs, exists := pp.streamVerify[reqID]
	pp.mu.Unlock()
	if !exists || vs == nil {
		t.Fatal("enforcement state must exist after the first verified call")
	}

	pp.EndRequest(reqID)
	pp.mu.Lock()
	_, exists = pp.streamVerify[reqID]
	pp.mu.Unlock()
	if exists {
		t.Fatal("enforcement state must be dropped by EndRequest")
	}
}

// TestStreamEnforcementNoStreamHooksSkipsState: a pipeline whose plugins
// have no run_on_stream_chunk hook must not create enforcement state and must
// pass events straight through.
func TestStreamEnforcementNoStreamHooksSkipsState(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-blocker/plugin.wasm") // run_before_request only
	pp := newTestPipeline(t, fixturesDir, []string{"test-blocker"})
	const reqID = 7005

	runVerified(t, pp, reqID, engine.StreamEvent{TextDelta: strPtr("hello")})
	pp.mu.Lock()
	_, exists := pp.streamVerify[reqID]
	pp.mu.Unlock()
	if exists {
		t.Fatal("no enforcement state expected without run_on_stream_chunk plugins")
	}
	if err := pp.EndStreamVerified(reqID); err != nil {
		t.Fatalf("EndStreamVerified on a no-hook pipeline must be a no-op: %v", err)
	}
	pp.EndRequest(reqID)
}

// TestStreamEnforcementMultiPluginNoDispatchAfterTerminal: with two stream
// plugins, the violation is attributed to the plugin whose scope closed (the
// rewriter), the terminal stops the call before the LATER plugin (the
// mutator) processes the close event, and every subsequent call returns the
// same terminal without dispatching anything.
func TestStreamEnforcementMultiPluginNoDispatchAfterTerminal(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-tool-rewriter/plugin.wasm")
	requireWASM(t, fixturesDir+"/test-stream-mutator/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-tool-rewriter", "test-stream-mutator"})
	const reqID = 7006

	events := []engine.StreamEvent{
		{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "call_1", Name: "search", Signature: streamSigA}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{"q":"original"}`}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 0}},
	}
	for _, ev := range events {
		e := ev
		if _, err := pp.RunOnStreamChunkVerified(context.Background(), reqID, &e); err != nil {
			// The terminal must arrive exactly at the block close (the third
			// event) and be attributed to the rewriting plugin.
			if ev.ToolCallEnd == nil {
				t.Fatalf("unexpected terminal before the block close: %v", err)
			}
			var term *StreamTerminalError
			if !errors.As(err, &term) || term.Plugin != "test-tool-rewriter" {
				t.Fatalf("expected rewriter attribution, got %T: %v", err, err)
			}
			// The mutator (a later plugin) must not run after the terminal:
			// its redaction would have rewritten a "secret" delta if it had.
			_, again := pp.RunOnStreamChunkVerified(context.Background(), reqID, &engine.StreamEvent{TextDelta: strPtr("the secret plan")})
			if !errors.As(again, &term) || term == nil {
				t.Fatalf("expected the recorded terminal on the next call, got %v", again)
			}
			pp.EndRequest(reqID)
			return
		}
	}
	t.Fatal("stale signature must terminate the stream at the block close")
}
