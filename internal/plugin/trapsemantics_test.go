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

// The same guarantees on the malformed-result path, driven through a real
// guest rather than the host's own helpers.
//
// A handwritten guest can issue host calls and THEN return an invalid frame —
// the SDK's typed results make that unrepresentable, so only a guest like this
// reaches the decode-error branch. The previous version of this test called
// RecordBlockVerdictForTest / discardTrapped / blocked directly, so forgetting
// either the discard or the break in that branch would have left it green:
// the exact regression it was asked to catch.
func TestBlockSurvivesAMalformedResultAndStopsDownstream(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-malformed-result/plugin.wasm")
	requireWASM(t, fixturesDir+"/test-records-invocation/plugin.wasm")

	pp := newTestPipeline(t, fixturesDir,
		[]string{"test-malformed-result", "test-records-invocation"})

	const reqID = 43
	chat := &engine.ChatRequest{
		Model:    "gpt-x",
		Messages: []engine.Message{{Role: engine.RoleUser, Content: "hello"}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), reqID, chat)
	if err != nil {
		t.Fatalf("failure_mode is pass, so a malformed result must not error: %v", err)
	}

	v := pp.Verdicts(reqID)
	if v == nil || v.Block() == nil {
		t.Fatal("the block did not survive a malformed result; a security verdict " +
			"must fail closed even when the guest then returns garbage")
	}
	if got := v.Block().Code; got != "malformed_guest" {
		t.Errorf("block code = %q, want the one the guest recorded", got)
	}
	if v.Respond() != nil {
		t.Error("a respond verdict from a guest that returned a malformed frame was kept")
	}

	model := chat.Model
	if out != nil {
		model = out.Model
	}
	if strings.Contains(model, "downstream-ran") {
		t.Fatal("a downstream plugin ran after a blocked request whose guest then " +
			"returned a malformed frame")
	}
}
