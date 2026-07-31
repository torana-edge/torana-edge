package plugin

import (
	"context"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

// ---------------------------------------------------------------------------
// Pipeline tests, finding 1: test-invented-content and provider presence.
//
// The fixture invents Content="invented" whenever it sees a non-nil Message on
// a mutable path. Whether that replacement is legal is the host's relative
// policy: content PRESENCE is host-owned, so on a body that actually has a
// text slot the value change is accepted, and on a body WITHOUT one the
// replacement invents a slot and must be rejected wholesale.
// ---------------------------------------------------------------------------

// A present content value is a legal mutation target: the fixture's value
// change (hi -> invented) is accepted and applied.
func TestRunAfterResponseInventedContentAccepted(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-invented-content/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-invented-content"})

	original := "hi"
	resp := &engine.ChatResponse{
		Message: &engine.ResponseMessage{Content: &original},
	}
	out, err := pp.RunAfterResponse(context.Background(), 80, resp, true)
	if err != nil {
		t.Fatalf("a legal content value change must be accepted: %v", err)
	}
	if out == nil || out.Message == nil {
		t.Fatal("result lost the assistant message")
	}
	if got := *out.Message.Content; got != "invented" {
		t.Errorf("content = %q, want %q (fixture's replacement was not applied)", got, "invented")
	}
}

// No text slot means inventing one is a presence violation: under allow mode
// the replacement is rejected atomically — no error, and Content stays nil.
func TestRunAfterResponseInventedContentPresenceRejected(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-invented-content/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-invented-content"})

	// Message present but Content absent: the provider body has no writable
	// text slot, so "invented" would fabricate one.
	resp := &engine.ChatResponse{
		Message: &engine.ResponseMessage{Content: nil},
	}
	out, err := pp.RunAfterResponse(context.Background(), 81, resp, true)
	if err != nil {
		t.Fatalf("allow mode must not error on a rejected presence change: %v", err)
	}
	if out == nil || out.Message == nil {
		t.Fatal("result lost the assistant message")
	}
	if out.Message.Content != nil {
		t.Errorf("content was invented where the provider had none: %q", *out.Message.Content)
	}
}

// The same presence violation under a block override is an attributed error
// naming the plugin, with the original response returned alongside it.
func TestRunAfterResponseInventedContentBlockAttributed(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-invented-content/plugin.wasm")

	rt := wasm.NewRuntime(context.Background())
	defer rt.Close()
	digest, err := BundleDigestForDir(fixturesDir + "/test-invented-content")
	if err != nil {
		t.Fatal(err)
	}
	pp, err := NewPipeline(rt, PluginConfig{
		Dir:   fixturesDir,
		Order: []string{"test-invented-content"},
		Approvals: map[string]Approval{
			// The manifest says pass; the approval overrides to block, which
			// is how an operator would pin this fixture's failure behaviour.
			"torana-test/test-invented-content": {Digest: digest, FailureMode: "block"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pp.Len() != 1 {
		t.Fatalf("loaded %d plugins, want 1", pp.Len())
	}

	// Content absent: the fixture's invented slot is a violation, which is
	// what block mode must surface.
	resp := &engine.ChatResponse{
		Message: &engine.ResponseMessage{Content: nil},
	}
	out, err := pp.RunAfterResponse(context.Background(), 82, resp, true)
	if err == nil {
		t.Fatal("block mode must return an error for an invented content slot")
	}
	if !strings.Contains(err.Error(), "test-invented-content") {
		t.Errorf("error %q does not name the offending plugin", err)
	}
	if !strings.Contains(err.Error(), "invalid response replacement") {
		t.Errorf("error %q does not describe the invalid replacement", err)
	}
	if out != resp {
		t.Errorf("blocked call should return the original response unchanged")
	}
}

// ---------------------------------------------------------------------------
// Pipeline tests, finding 2: host-owned field forgery is rejected and never
// poisons the chain.
//
// test-forge-response-fields rewrites ToolCalls[0].Id and Signature — two
// fields a guest is not allowed to touch. The host must refuse that
// replacement BEFORE it can become the next plugin's input, so a downstream
// mutator still sees (and rewrites) the ORIGINAL calls.
// ---------------------------------------------------------------------------

// Under allow mode the forged replacement is skipped and the accepted response
// chains onward: test-mutator rewrites both calls' arguments in place while
// their host-owned ids (and the absent signatures) survive untouched.
func TestRunAfterResponseForgeRejectedNoPoison(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-forge-response-fields/plugin.wasm")
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")

	pp := newTestPipeline(t, fixturesDir, []string{"test-forge-response-fields", "test-mutator"})

	original := "hi"
	resp := &engine.ChatResponse{
		Message: &engine.ResponseMessage{
			Content: &original,
			ToolCalls: []engine.ResponseToolCall{
				{ID: "call_a", Name: "alpha", ArgumentsJSON: []byte(`{"a":1}`)},
				{ID: "call_b", Name: "beta", ArgumentsJSON: []byte(`{"b":2}`)},
			},
		},
	}
	out, err := pp.RunAfterResponse(context.Background(), 83, resp, true)
	if err != nil {
		t.Fatalf("allow mode must not error: %v", err)
	}
	if out == nil || out.Message == nil {
		t.Fatal("result lost the assistant message")
	}
	if len(out.Message.ToolCalls) != 2 {
		t.Fatalf("tool-call count = %d, want 2", len(out.Message.ToolCalls))
	}
	// The mutator rewrote BOTH calls, which proves the chain ran normally —
	// the poison check is the host-owned fields below.
	wantArgs := `{"mutated_by":"test-mutator"}`
	for i, tc := range out.Message.ToolCalls {
		if string(tc.ArgumentsJSON) != wantArgs {
			t.Errorf("tool call %d arguments = %s, want %s", i, tc.ArgumentsJSON, wantArgs)
		}
	}
	// The host-owned fields must carry the ORIGINAL values: the guest's
	// forged-id / forged-sig were refused, not applied.
	wantIDs := []string{"call_a", "call_b"}
	for i, wantID := range wantIDs {
		if got := out.Message.ToolCalls[i].ID; got != wantID {
			t.Errorf("tool call %d id = %q, want %q (forged id leaked)", i, got, wantID)
		}
		if got := out.Message.ToolCalls[i].Signature; got != "" {
			t.Errorf("tool call %d signature = %q, want empty (forged signature leaked)", i, got)
		}
	}
}

// The same forgery under a block override is an attributed error naming the
// plugin, with the original response returned alongside it.
func TestRunAfterResponseForgeBlockAttributed(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-forge-response-fields/plugin.wasm")

	rt := wasm.NewRuntime(context.Background())
	defer rt.Close()
	digest, err := BundleDigestForDir(fixturesDir + "/test-forge-response-fields")
	if err != nil {
		t.Fatal(err)
	}
	pp, err := NewPipeline(rt, PluginConfig{
		Dir:   fixturesDir,
		Order: []string{"test-forge-response-fields"},
		Approvals: map[string]Approval{
			"torana-test/test-forge-response-fields": {Digest: digest, FailureMode: "block"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pp.Len() != 1 {
		t.Fatalf("loaded %d plugins, want 1", pp.Len())
	}

	original := "hi"
	resp := &engine.ChatResponse{
		Message: &engine.ResponseMessage{
			Content: &original,
			ToolCalls: []engine.ResponseToolCall{
				{ID: "call_a", Name: "alpha", ArgumentsJSON: []byte(`{"a":1}`)},
			},
		},
	}
	out, err := pp.RunAfterResponse(context.Background(), 84, resp, true)
	if err == nil {
		t.Fatal("block mode must return an error for a forged host-owned field")
	}
	if !strings.Contains(err.Error(), "test-forge-response-fields") {
		t.Errorf("error %q does not name the offending plugin", err)
	}
	if !strings.Contains(err.Error(), "invalid response replacement") {
		t.Errorf("error %q does not describe the invalid replacement", err)
	}
	if out != resp {
		t.Errorf("blocked call should return the original response unchanged")
	}
}
