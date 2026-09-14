package plugin

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

func journalPipeline(t *testing.T, mode string, limit uint64) *PluginPipeline {
	t.Helper()
	requireWASM(t, fixturesDir+"/test-stream-journal/plugin.wasm")
	rt := wasm.NewRuntimeWithOptions(context.Background(), wasm.RuntimeOptions{MaxStreamBufferBytes: limit})
	t.Cleanup(func() { rt.Close() })
	pp, err := NewPipeline(rt, PluginConfig{Dir: fixturesDir, Order: []string{"test-stream-journal"}, AllowUnapproved: true})
	if err != nil {
		t.Fatal(err)
	}
	if pp.Len() != 1 {
		t.Fatal("missing journal guest")
	}
	pp.streamPlugins[0].failureMode = mode
	return pp
}

// An upstream stream that ends with an open tool block is a host-side
// terminal error. The journal guest may have buffered every accepted event,
// but neither pass nor block failure mode may turn those unverified events
// into output during finalization.
func TestStreamJournalEndRejectsOpenToolWithoutReleasingJournal(t *testing.T) {
	for _, mode := range []string{"pass", "block"} {
		t.Run(mode, func(t *testing.T) {
			pp := journalPipeline(t, mode, 0)
			const reqID = 41
			for _, ev := range []engine.StreamEvent{toolStart(0, "call", "read"), toolDelta(0, `{"path":"open.go"}`)} {
				event := ev
				out, err := pp.RunOnStreamChunkVerified(context.Background(), reqID, &event)
				if err != nil || len(out) != 0 {
					t.Fatalf("pending journal event escaped: out=%#v err=%v", out, err)
				}
			}
			err := pp.EndStreamVerified(reqID)
			var terminal *StreamTerminalError
			if !errors.As(err, &terminal) || terminal.Kind != streamTerminalHost || terminal.Plugin != "host" {
				t.Fatalf("end terminal = %T: %v, want typed host terminal", err, err)
			}
			if !strings.Contains(terminal.Error(), "missing ContentBlockStop") {
				t.Fatalf("terminal lacks open-block reason: %v", terminal)
			}
			if state := pp.streamVerify[reqID]; state == nil || state.plugins[0].journal == nil || len(state.plugins[0].journal.originals) != 2 {
				t.Fatalf("journal was released or discarded before terminal: %#v", state)
			}
			if err2 := pp.EndStreamVerified(reqID); err2 != err {
				t.Fatalf("terminal finalization was not sticky: first=%v second=%v", err, err2)
			}
			pp.EndRequest(reqID)
		})
	}
}

func multiJournalPipeline(t *testing.T, downstream string) *PluginPipeline {
	t.Helper()
	requireWASM(t, fixturesDir+"/test-stream-journal/plugin.wasm")
	requireWASM(t, fixturesDir+"/"+downstream+"/plugin.wasm")
	rt := wasm.NewRuntimeWithOptions(context.Background(), wasm.RuntimeOptions{})
	t.Cleanup(func() { rt.Close() })
	pp, err := NewPipeline(rt, PluginConfig{Dir: fixturesDir, Order: []string{"test-stream-journal", downstream}, AllowUnapproved: true})
	if err != nil {
		t.Fatal(err)
	}
	if pp.Len() != 2 || len(pp.streamPlugins) != 2 {
		t.Fatalf("loaded pipeline = %d/%d, want two stream plugins", pp.Len(), len(pp.streamPlugins))
	}
	return pp
}

func TestStreamJournalPassReplaysInterleavedCallsExactlyOnce(t *testing.T) {
	pp := journalPipeline(t, "pass", 0)
	input := []engine.StreamEvent{toolStart(0, "call_0", "one"), toolDelta(0, `{"a":`), toolStart(1, "call_1", "two"), toolDelta(1, `{}`)}
	for _, ev := range input {
		if out := run(t, pp, ev); len(out) != 0 {
			t.Fatalf("released deferred output: %#v", out)
		}
	}
	fail := toolDelta(0, "FAIL")
	out := run(t, pp, fail)
	want := append(input, fail)
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("replay lost or reordered original events:\n%#v\nwant %#v", out, want)
	}
	for _, ev := range []engine.StreamEvent{toolEnd(1), toolEnd(0)} {
		if got := run(t, pp, ev); !reflect.DeepEqual(got, []engine.StreamEvent{ev}) {
			t.Fatalf("guest resumed after rollback: %#v", got)
		}
	}
	text := "original text"
	ev := engine.StreamEvent{TextDelta: &text}
	if got := run(t, pp, ev); !reflect.DeepEqual(got, []engine.StreamEvent{ev}) {
		t.Fatalf("disabled guest mutated text: %#v", got)
	}
	if err := pp.EndStreamVerified(1); err != nil {
		t.Fatal(err)
	}
	pp.EndRequest(1)
	// A new request still invokes the guest; disabling is request-scoped.
	if got := runAs(t, pp, 2, toolStart(0, "fresh", "one")); len(got) != 0 {
		t.Fatal("rollback disabled guest globally")
	}
	pp.EndRequest(2)
}

func TestStreamJournalBlockReleasesNoDeferredEvents(t *testing.T) {
	pp := journalPipeline(t, "block", 0)
	for _, ev := range []engine.StreamEvent{toolStart(0, "call_0", "one"), toolDelta(0, `{"a":`)} {
		if len(run(t, pp, ev)) != 0 {
			t.Fatal("released deferred event")
		}
	}
	fail := toolDelta(0, "FAIL")
	out, err := pp.RunOnStreamChunkVerified(context.Background(), 1, &fail)
	if err == nil || len(out) != 0 {
		t.Fatalf("block must terminate with no output: %v %#v", err, out)
	}
	out, err = pp.RunOnStreamChunkVerified(context.Background(), 1, &fail)
	if err == nil || len(out) != 0 {
		t.Fatal("terminal request resumed")
	}
	pp.EndRequest(1)
}

func TestStreamJournalBudgetFallsBackWithoutTruncation(t *testing.T) {
	pp := journalPipeline(t, "pass", 256)
	start := toolStart(0, "call", "one")
	if len(run(t, pp, start)) != 0 {
		t.Fatal("start not held")
	}
	delta := toolDelta(0, strings.Repeat("x", 256))
	if got := run(t, pp, delta); !reflect.DeepEqual(got, []engine.StreamEvent{start, delta}) {
		t.Fatalf("overflow truncated accepted stream: %#v", got)
	}
	if len(run(t, pp, toolEnd(0))) != 1 {
		t.Fatal("stop lost")
	}
	if err := pp.EndStreamVerified(1); err != nil {
		t.Fatal(err)
	}
}

func TestStreamJournalDeliberateSuppressionCommits(t *testing.T) {
	pp := journalPipeline(t, "pass", 0)
	for _, ev := range []engine.StreamEvent{toolStart(0, "call", "one"), toolDelta(0, `{}`), toolEnd(0)} {
		if out := run(t, pp, ev); len(out) != 0 {
			t.Fatalf("whole tool suppression emitted: %#v", out)
		}
	}
	if err := pp.EndStreamVerified(1); err != nil {
		t.Fatal(err)
	}
	if pp.streamVerify[1].plugins[0].journal != nil {
		t.Fatal("completed journal retained")
	}
}

// A pass-mode rollback is the upstream plugin's output, not a shortcut around
// the remaining pipeline. The restored events must traverse the downstream
// plugin exactly once, and any downstream signature violation must retain its
// normal terminal semantics. Only the journal guest is disabled by rollback.
func TestStreamJournalRollbackTraversesDownstreamOnceAndPreservesTerminalPolicy(t *testing.T) {
	pp := multiJournalPipeline(t, "test-tool-rewriter")
	pp.streamPlugins[0].failureMode = "pass"
	pp.streamPlugins[1].failureMode = "block"

	start := engine.StreamEvent{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "call", Name: "lookup", Signature: "provider-signature"}}
	if out := run(t, pp, start); len(out) != 0 {
		t.Fatalf("journal start escaped before rollback: %#v", out)
	}
	fail := toolDelta(0, "FAIL")
	out := run(t, pp, fail)
	if len(out) != 2 || out[0].ToolCallStart == nil || out[1].ToolCallDelta == nil {
		t.Fatalf("rollback did not traverse downstream as one ordered replay: %#v", out)
	}
	const rewritten = `{"q":"rewritten-by-plugin"}`
	if out[1].ToolCallDelta.ArgumentsDelta != rewritten {
		t.Fatalf("downstream rewriter did not see rollback delta: %#v", out[1])
	}
	// One start and one rewritten delta proves the rollback was not forwarded
	// both directly and through the downstream stage.
	if out[0].ToolCallStart.ID != "call" || out[0].ToolCallStart.Signature != "provider-signature" {
		t.Fatalf("original signed start was not restored exactly: %#v", out[0])
	}

	end := toolEnd(0)
	if emitted, err := pp.RunOnStreamChunkVerified(context.Background(), 1, &end); err == nil || len(emitted) != 0 {
		t.Fatalf("downstream signed mutation must terminate without output: emitted=%#v err=%v", emitted, err)
	} else if terminal, ok := err.(*StreamTerminalError); !ok || terminal.Plugin != "test-tool-rewriter" {
		t.Fatalf("terminal attribution = %#v, want downstream plugin", err)
	}
	// Terminal state short-circuits the request after the downstream block.
	if emitted, err := pp.RunOnStreamChunkVerified(context.Background(), 1, &end); err == nil || len(emitted) != 0 {
		t.Fatalf("terminal request resumed: emitted=%#v err=%v", emitted, err)
	}
	if state := pp.streamVerify[1]; state == nil || !state.plugins[0].disabled || state.plugins[1].disabled {
		t.Fatalf("rollback disabled wrong guests: %#v", state)
	}

	pp.EndRequest(1)
	if out := runAs(t, pp, 2, toolStart(0, "fresh", "lookup")); len(out) != 0 {
		t.Fatalf("request-scoped rollback disabled journal globally: %#v", out)
	}
	pp.EndRequest(2)
}
