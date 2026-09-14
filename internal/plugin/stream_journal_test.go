package plugin

import (
	"context"
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
