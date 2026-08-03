package plugin

import (
	"context"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// The accepted-input closure at the pipeline (review finding 2): an engine
// request outside the SDK replacement domain fails BEFORE the first hook —
// no plugin ever observes or mutates it — and the zero-plugin path fails
// with a host-local input error instead of truncating or silently dropping
// a fact.

// TestRunBeforeRequestRefusesInvalidInputBeforeAnyHook — the recording
// fixture tags the model when its hook runs; an invalid engine request must
// fail without it ever being invoked.
func TestRunBeforeRequestRefusesInvalidInputBeforeAnyHook(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-records-invocation/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-records-invocation"})

	cases := map[string]*engine.ChatRequest{
		"zero-arm block": {
			Model:    "gpt-x",
			Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{}}}},
		},
		"multi-arm block": {
			Model: "gpt-x",
			Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{
				{Text: &engine.TextBlock{Text: "a"}, ToolUse: &engine.ToolUseBlock{ID: "c1", Name: "r", Arguments: mustReqForTest(`{}`)}},
			}}},
		},
		"max tokens out of range": {
			Model:     "gpt-x",
			MaxTokens: new(int(1) << 40),
			Messages:  []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}}},
		},
	}
	for name, chat := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := pp.RunBeforeRequest(context.Background(), 99, chat, nil)
			if err == nil {
				t.Fatalf("invalid engine input must fail before hooks: %+v", out)
			}
			if !strings.Contains(err.Error(), "invalid engine request") {
				t.Fatalf("error = %q, want the input-closure condition named", err)
			}
			if strings.Contains(chat.Model, "+downstream-ran") {
				t.Fatal("a hook ran on invalid input")
			}
		})
	}
}

// TestRunBeforeRequestNoPluginPathRefusesInvalidInput — with ZERO plugins
// the entry conversion still runs: invalid input is a host-local error, not
// a silent truncation on the way to marshal.
func TestRunBeforeRequestNoPluginPathRefusesInvalidInput(t *testing.T) {
	pp := newTestPipeline(t, fixturesDir, []string{})

	valid := &engine.ChatRequest{
		Model:    "gpt-x",
		Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 1, valid, nil)
	if err != nil {
		t.Fatalf("valid input through the no-plugin path: %v", err)
	}
	if out != valid {
		t.Fatal("the no-plugin path must return the request unchanged")
	}

	invalid := &engine.ChatRequest{
		Model:    "gpt-x",
		Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{}}}},
	}
	if _, err := pp.RunBeforeRequest(context.Background(), 2, invalid, nil); err == nil {
		t.Fatal("invalid input through the no-plugin path must fail locally")
	}
}

// TestRunBeforeRequestNoPluginPathPreservesUnchangedRequest — a valid
// request through the no-plugin path is byte-identical afterwards (no
// conversion round-trip, no normalization surprises).
func TestRunBeforeRequestNoPluginPathPreservesUnchangedRequest(t *testing.T) {
	pp := newTestPipeline(t, fixturesDir, []string{})

	chat := &engine.ChatRequest{
		Model:    "gpt-x",
		Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}}},
		Tools:    []engine.ToolDef{{Name: "read", Parameters: mustReqForTest(`{"type":"object"}`)}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 3, chat, nil)
	if err != nil {
		t.Fatalf("no-plugin path: %v", err)
	}
	if out != chat {
		t.Fatal("no-plugin path must return the exact same request")
	}
}

func mustReqForTest(raw string) engine.RequiredJSONObject {
	r, err := engine.ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		panic(err)
	}
	return r
}
