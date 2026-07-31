package plugin

import (
	"context"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// A block recorded before a trap must survive AND short-circuit.
//
// This is the security-critical case. The block check originally sat at the
// bottom of the loop body, so a plugin that blocked and then trapped hit
// `continue` and every downstream plugin still received the request — exactly
// the PII-keeps-flowing problem short-circuiting exists to stop. The control
// flow looked right; only a two-plugin test can show it.
func TestBlockSurvivesATrapAndStopsDownstreamPlugins(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-block-then-trap/plugin.wasm")
	requireWASM(t, fixturesDir+"/test-records-invocation/plugin.wasm")

	pp := newTestPipeline(t, fixturesDir,
		[]string{"test-block-then-trap", "test-records-invocation"})

	const reqID = 42
	chat := &engine.ChatRequest{
		Model:    "gpt-x",
		Messages: []engine.Message{{Role: engine.RoleUser, Content: "hello"}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), reqID, chat)
	if err != nil {
		t.Fatalf("failure_mode is pass, so a trap must not error: %v", err)
	}

	v := pp.Verdicts(reqID)
	if v == nil || v.Block() == nil {
		t.Fatal("the block did not survive the trap; a security verdict must fail closed")
	}
	if got := v.Block().Code; got != "blocked_then_trapped" {
		t.Errorf("block code = %q, want the one the plugin recorded", got)
	}
	if got := v.Block().Plugin; got != "test-block-then-trap" {
		t.Errorf("block attributed to %q", got)
	}

	// The respond verdict came from the same plugin and must be discarded: a
	// half-built synthetic response from code that crashed immediately after
	// is not trustworthy.
	if v.Respond() != nil {
		t.Error("a respond verdict from a plugin that then trapped was kept")
	}

	// The downstream plugin tags the model when it runs.
	model := chat.Model
	if out != nil {
		model = out.Model
	}
	if strings.Contains(model, "downstream-ran") {
		t.Fatal("a downstream plugin ran after the request was blocked — this is the " +
			"PII-keeps-flowing bug the short-circuit exists to prevent")
	}
}

// The same guarantees on the malformed-result path. A handwritten guest can
// issue host calls and THEN return an invalid frame, so that path needs the
// same treatment as a trap rather than only the trap being handled.
func TestBlockSurvivesAMalformedResultAndStopsDownstream(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-records-invocation/plugin.wasm")

	pp := newTestPipeline(t, fixturesDir, []string{"test-records-invocation"})

	// Record the verdicts directly, then drive the discard path the pipeline
	// uses for a refused result. Compiling a guest that returns deliberately
	// malformed bytes would pin the same behaviour through more machinery
	// without testing anything extra.
	const reqID = 43
	pp.runtime.RecordBlockVerdictForTest(reqID, "handwritten", 422, "malformed", "refused")
	pp.runtime.RecordRespondVerdictForTest(reqID, "handwritten", "discard me")

	pp.discardTrapped(reqID, "handwritten")

	v := pp.Verdicts(reqID)
	if v.Block() == nil {
		t.Fatal("the block did not survive a refused result")
	}
	if v.Respond() != nil {
		t.Error("a respond verdict survived a refused result")
	}
	if !pp.blocked(reqID) {
		t.Fatal("blocked() did not see the surviving block, so the pipeline would " +
			"not short-circuit")
	}
}
