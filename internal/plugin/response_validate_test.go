package plugin

import (
	"context"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// ---------------------------------------------------------------------------
// Unit tests: validateResponseReplacement over hand-built pbv2 values.
// ---------------------------------------------------------------------------

func strPtr(s string) *string { return &s }

// pbv2Msg builds a ResponseMessage with the given content presence and the
// requested number of structurally valid tool calls.
func pbv2Msg(content *string, nCalls int) *pbv2.ResponseMessage {
	m := &pbv2.ResponseMessage{Content: content}
	for i := 0; i < nCalls; i++ {
		m.ToolCalls = append(m.ToolCalls, &pbv2.ToolCall{
			Id:            "call_" + string(rune('a'+i)),
			Name:          "t",
			ArgumentsJson: []byte(`{}`),
		})
	}
	return m
}

func pbv2Resp(content *string, nCalls int) *pbv2.ChatResponse {
	return &pbv2.ChatResponse{Message: pbv2Msg(content, nCalls)}
}

func TestValidateResponseReplacementNilReplacement(t *testing.T) {
	current := pbv2Resp(strPtr("hi"), 1)
	if err := validateResponseReplacement(current, nil); err != nil {
		t.Errorf("nil replacement must pass through: %v", err)
	}
}

func TestValidateResponseReplacementMessagePresence(t *testing.T) {
	t.Run("dropped", func(t *testing.T) {
		// A response IS one message; a replacement with no message at all is
		// a structural lie, not a mutation.
		current := pbv2Resp(nil, 1)
		replacement := &pbv2.ChatResponse{}
		err := validateResponseReplacement(current, replacement)
		if err == nil {
			t.Fatal("dropping the assistant message must be rejected")
		}
		if !strings.Contains(err.Error(), "dropped the assistant message") {
			t.Errorf("error %q does not name the dropped message", err)
		}
	})
	t.Run("invented", func(t *testing.T) {
		current := &pbv2.ChatResponse{}
		replacement := pbv2Resp(nil, 0)
		err := validateResponseReplacement(current, replacement)
		if err == nil {
			t.Fatal("inventing an assistant message where the accepted response had none must be rejected")
		}
		if !strings.Contains(err.Error(), "invented an assistant message") {
			t.Errorf("error %q does not name the invented message", err)
		}
	})
	t.Run("both absent", func(t *testing.T) {
		if err := validateResponseReplacement(&pbv2.ChatResponse{}, &pbv2.ChatResponse{}); err != nil {
			t.Errorf("both absent must be valid: %v", err)
		}
	})
}

// CRITERION 3: unequal tool-call counts are rejected — the SDK only validates
// the replacement in isolation, so the fixed cardinality against the accepted
// response is the host's check.
func TestValidateResponseReplacementToolCallCardinality(t *testing.T) {
	cases := []struct {
		name             string
		currentCalls     int
		replacementCalls int
	}{
		{"2 vs 1", 2, 1},
		{"1 vs 0", 1, 0},
		{"0 vs 1", 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateResponseReplacement(
				pbv2Resp(nil, tc.currentCalls),
				pbv2Resp(nil, tc.replacementCalls),
			)
			if err == nil {
				t.Fatalf("cardinality change %d -> %d must be rejected", tc.currentCalls, tc.replacementCalls)
			}
			if !strings.Contains(err.Error(), "changed tool-call cardinality") {
				t.Errorf("error %q does not name the cardinality change", err)
			}
		})
	}
	t.Run("equal counts allowed", func(t *testing.T) {
		if err := validateResponseReplacement(pbv2Resp(nil, 2), pbv2Resp(nil, 2)); err != nil {
			t.Errorf("equal cardinality must be valid: %v", err)
		}
	})
}

// CRITERION 4: content PRESENCE is host-owned and fixed across accepted ->
// output, so flipping it in either direction is rejected. Present-empty and
// present-nonempty are the same presence — only the value may change.
func TestValidateResponseReplacementContentPresence(t *testing.T) {
	cases := []struct {
		name               string
		currentContent     *string
		replacementContent *string
		wantRejected       bool
	}{
		{"present to absent", strPtr("hi"), nil, true},
		{"absent to present", nil, strPtr("hi"), true},
		{"present-empty to present-nonempty", strPtr(""), strPtr("hi"), false},
		{"present-nonempty to present-empty", strPtr("hi"), strPtr(""), false},
		{"absent to absent", nil, nil, false},
		{"same value", strPtr("hi"), strPtr("hi"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateResponseReplacement(
				pbv2Resp(tc.currentContent, 0),
				pbv2Resp(tc.replacementContent, 0),
			)
			if tc.wantRejected {
				if err == nil {
					t.Fatal("content-presence change must be rejected")
				}
				if !strings.Contains(err.Error(), "changed content presence") {
					t.Errorf("error %q does not name the presence change", err)
				}
			} else if err != nil {
				t.Errorf("must be accepted: %v", err)
			}
		})
	}
}

// CRITERION 6 (unit level): rejection is wholesale. A replacement that changes
// only the content VALUE (legal on its own) AND drops a tool call (illegal) is
// refused entirely on the cardinality violation — nothing about it is accepted
// piecemeal.
func TestValidateResponseReplacementAtomicRejection(t *testing.T) {
	current := pbv2Resp(strPtr("original"), 2)
	replacement := pbv2Resp(strPtr("poisoned-content"), 1)
	err := validateResponseReplacement(current, replacement)
	if err == nil {
		t.Fatal("a replacement that drops a tool call must be rejected even when its other changes are individually legal")
	}
	if !strings.Contains(err.Error(), "changed tool-call cardinality") {
		t.Errorf("error %q does not report the cardinality violation", err)
	}
}

// ---------------------------------------------------------------------------
// Pipeline tests: real fixtures through RunAfterResponse.
// ---------------------------------------------------------------------------

// CRITERION 5a + 6: an invalid replacement (poisoned content + dropped tool
// call) in allow mode must never become the downstream plugin's input or the
// output: content stays unchanged and both tool calls survive, then the next
// plugin rewrites them in place.
func TestRunAfterResponseInvalidReplacementAllowNoPoison(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-invalid-replacement/plugin.wasm")
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")

	pp := newTestPipeline(t, fixturesDir, []string{"test-invalid-replacement", "test-mutator"})

	const reqID = 77
	original := "original-content"
	resp := &engine.ChatResponse{
		Message: &engine.ResponseMessage{
			Content: &original,
			ToolCalls: []engine.ResponseToolCall{
				{ID: "call_1", Name: "alpha", ArgumentsJSON: []byte(`{"a":1}`)},
				{ID: "call_2", Name: "beta", ArgumentsJSON: []byte(`{"b":2}`)},
			},
		},
	}
	out, err := pp.RunAfterResponse(context.Background(), reqID, resp, true)
	if err != nil {
		t.Fatalf("allow mode must not error: %v", err)
	}
	if out == nil || out.Message == nil {
		t.Fatal("result lost the assistant message")
	}
	// CRITERION 6: the invented content and the call-drop were refused
	// atomically — neither may appear in the output.
	if got := *out.Message.Content; got != original {
		t.Errorf("content = %q, want unchanged %q (poisoned replacement leaked)", got, original)
	}
	if len(out.Message.ToolCalls) != 2 {
		t.Fatalf("tool-call count = %d, want 2 (the dropped call must still be present)", len(out.Message.ToolCalls))
	}
	// CRITERION 5a: the next plugin saw the ACCEPTED response, so its in-place
	// rewrite reached both calls. If the invalid replacement had been chained
	// downstream, only one call would have been rewritten.
	wantArgs := `{"mutated_by":"test-mutator"}`
	for i, tc := range out.Message.ToolCalls {
		if string(tc.ArgumentsJSON) != wantArgs {
			t.Errorf("tool call %d arguments = %s, want %s", i, tc.ArgumentsJSON, wantArgs)
		}
	}
}

// CRITERION 5b: the same invalid replacement under failure_mode block is an
// attributed error naming the plugin, and the original response is returned
// alongside it.
func TestRunAfterResponseInvalidReplacementBlockAttributed(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-invalid-replacement/plugin.wasm")

	rt := wasm.NewRuntime(context.Background())
	defer rt.Close()
	digest, err := BundleDigestForDir(fixturesDir + "/test-invalid-replacement")
	if err != nil {
		t.Fatal(err)
	}
	pp, err := NewPipeline(rt, PluginConfig{
		Dir:   fixturesDir,
		Order: []string{"test-invalid-replacement"},
		Approvals: map[string]Approval{
			// The manifest says pass; the approval overrides to block, which
			// is how an operator would pin this fixture's failure behaviour.
			"torana-test/test-invalid-replacement": {Digest: digest, FailureMode: "block"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pp.Len() != 1 {
		t.Fatalf("loaded %d plugins, want 1", pp.Len())
	}

	const reqID = 78
	original := "original-content"
	resp := &engine.ChatResponse{
		Message: &engine.ResponseMessage{
			Content: &original,
			ToolCalls: []engine.ResponseToolCall{
				{ID: "call_1", Name: "alpha", ArgumentsJSON: []byte(`{"a":1}`)},
				{ID: "call_2", Name: "beta", ArgumentsJSON: []byte(`{"b":2}`)},
			},
		},
	}
	out, err := pp.RunAfterResponse(context.Background(), reqID, resp, true)
	if err == nil {
		t.Fatal("block mode must return an error for an invalid replacement")
	}
	if !strings.Contains(err.Error(), "test-invalid-replacement") {
		t.Errorf("error %q does not name the offending plugin", err)
	}
	if !strings.Contains(err.Error(), "invalid response replacement") {
		t.Errorf("error %q does not describe the invalid replacement", err)
	}
	if out != resp {
		t.Errorf("blocked call should return the original response unchanged")
	}
}
