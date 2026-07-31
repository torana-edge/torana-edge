package plugin

import (
	"context"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
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

// hostOwnedBase builds a fully-populated ChatResponse: every host-owned field
// set, plus one tool call with a bound signature. Tests clone it and mutate a
// single aspect, so a rejection can be attributed to exactly that change.
func hostOwnedBase() *pbv2.ChatResponse {
	content := "hi"
	return &pbv2.ChatResponse{
		Model:                  "gpt-4o",
		Id:                     "resp_42",
		FinishReason:           "stop",
		UpstreamStatus:         200,
		DurationMs:             1337,
		ProviderExtensionsJson: []byte(`{"safety":{"blocked":false}}`),
		Usage: &pbv2.Usage{
			InputTokens:      11,
			OutputTokens:     22,
			CacheReadTokens:  33,
			CacheWriteTokens: 44,
		},
		Message: &pbv2.ResponseMessage{
			Content: &content,
			ToolCalls: []*pbv2.ToolCall{
				{Id: "call_a", Name: "get_weather", ArgumentsJson: []byte(`{"city":"sf"}`), Signature: "tok_abc"},
			},
		},
	}
}

// CRITERION 2 (host-owned): every ChatResponse fact the host observed (model,
// id, finish reason, upstream status, duration, provider extensions) is
// immutable under plugin mutation. Identical re-emission passes; changing any
// single field rejects and names the field.
func TestValidateResponseReplacementHostOwnedChatResponseFields(t *testing.T) {
	base := hostOwnedBase()
	cases := []struct {
		name   string
		mutate func(*pbv2.ChatResponse)
	}{
		{"model", func(r *pbv2.ChatResponse) { r.Model = "gpt-3.5" }},
		{"id", func(r *pbv2.ChatResponse) { r.Id = "resp_forged" }},
		{"finish_reason", func(r *pbv2.ChatResponse) { r.FinishReason = "length" }},
		{"upstream_status", func(r *pbv2.ChatResponse) { r.UpstreamStatus = 500 }},
		{"duration_ms", func(r *pbv2.ChatResponse) { r.DurationMs = 1 }},
		{"provider_extensions_json", func(r *pbv2.ChatResponse) { r.ProviderExtensionsJson = []byte(`{"safety":{"blocked":true}}`) }},
	}
	for _, tc := range cases {
		t.Run(tc.name+" changed", func(t *testing.T) {
			replacement := proto.Clone(base).(*pbv2.ChatResponse)
			tc.mutate(replacement)
			err := validateResponseReplacement(base, replacement)
			if err == nil {
				t.Fatalf("changing host-owned field %s must be rejected", tc.name)
			}
			if want := "changed host-owned field " + tc.name; !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %s", err, tc.name)
			}
		})
	}
	t.Run("identical re-emission accepted", func(t *testing.T) {
		replacement := proto.Clone(base).(*pbv2.ChatResponse)
		if err := validateResponseReplacement(base, replacement); err != nil {
			t.Errorf("identical re-emission must be accepted: %v", err)
		}
	})
}

// CRITERION 2 (usage): the token tally is compared field-wise across all four
// counts, nil-safe. Dropping or inventing the whole Usage block is also a
// change — usage presence is a host-observed fact, so nil-vs-non-nil rejects.
func TestValidateResponseReplacementHostOwnedUsage(t *testing.T) {
	base := hostOwnedBase()
	counts := []struct {
		name string
		mut  func(*pbv2.Usage)
	}{
		{"input_tokens", func(u *pbv2.Usage) { u.InputTokens++ }},
		{"output_tokens", func(u *pbv2.Usage) { u.OutputTokens++ }},
		{"cache_read_tokens", func(u *pbv2.Usage) { u.CacheReadTokens++ }},
		{"cache_write_tokens", func(u *pbv2.Usage) { u.CacheWriteTokens++ }},
	}
	for _, tc := range counts {
		t.Run(tc.name+" changed", func(t *testing.T) {
			replacement := proto.Clone(base).(*pbv2.ChatResponse)
			tc.mut(replacement.Usage)
			err := validateResponseReplacement(base, replacement)
			if err == nil {
				t.Fatalf("changing usage.%s must be rejected", tc.name)
			}
			if want := "changed host-owned field usage"; !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name usage", err)
			}
		})
	}
	t.Run("usage dropped", func(t *testing.T) {
		replacement := proto.Clone(base).(*pbv2.ChatResponse)
		replacement.Usage = nil
		if err := validateResponseReplacement(base, replacement); err == nil {
			t.Fatal("dropping usage must be rejected")
		}
	})
	t.Run("usage invented", func(t *testing.T) {
		current := proto.Clone(base).(*pbv2.ChatResponse)
		current.Usage = nil
		if err := validateResponseReplacement(current, base); err == nil {
			t.Fatal("inventing usage where the provider reported none must be rejected")
		}
	})
	t.Run("both absent accepted", func(t *testing.T) {
		current := proto.Clone(base).(*pbv2.ChatResponse)
		current.Usage = nil
		replacement := proto.Clone(current).(*pbv2.ChatResponse)
		if err := validateResponseReplacement(current, replacement); err != nil {
			t.Errorf("both-absent usage must be accepted: %v", err)
		}
	})
	t.Run("identical tally accepted", func(t *testing.T) {
		if err := validateResponseReplacement(base, proto.Clone(base).(*pbv2.ChatResponse)); err != nil {
			t.Errorf("identical usage must be accepted: %v", err)
		}
	})
}

// CRITERION 2 (tool call id): the call's id is the provider's identity and is
// host-owned. Changing it at any position is rejected, naming the index.
func TestValidateResponseReplacementToolCallId(t *testing.T) {
	t.Run("single call", func(t *testing.T) {
		base := hostOwnedBase()
		replacement := proto.Clone(base).(*pbv2.ChatResponse)
		replacement.Message.ToolCalls[0].Id = "forged_id"
		err := validateResponseReplacement(base, replacement)
		if err == nil {
			t.Fatal("changing a tool call id must be rejected")
		}
		if want := "tool call 0 changed host-owned id"; !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name tool call 0's id", err)
		}
	})
	t.Run("second call indexed", func(t *testing.T) {
		current := hostOwnedBase()
		current.Message.ToolCalls = append(current.Message.ToolCalls,
			&pbv2.ToolCall{Id: "call_b", Name: "search", ArgumentsJson: []byte(`{}`)})
		replacement := proto.Clone(current).(*pbv2.ChatResponse)
		replacement.Message.ToolCalls[1].Id = "forged_b"
		err := validateResponseReplacement(current, replacement)
		if err == nil {
			t.Fatal("changing the second call's id must be rejected")
		}
		if want := "tool call 1 changed host-owned id"; !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name tool call 1's id", err)
		}
	})
}

// CRITERION 2 (signature matrix): the bound provider signature is host-owned
// except for TWO valid mutations on the response path — clearing it because
// the content it covers changed, and leaving it UNCHANGED over changed content
// (the apply block invalidates the token itself before it ships, so an intact
// signature never signs content the provider didn't). The outright frauds
// reject with the class named: dropped (cleared over unchanged content), forged
// (replaced with another non-empty token), added (minted where none existed).
func TestValidateResponseReplacementSignatureMatrix(t *testing.T) {
	base := hostOwnedBase() // call 0: signature "tok_abc"
	changeContent := func(r *pbv2.ChatResponse) {
		r.Message.ToolCalls[0].Name = "get_forecast"
		r.Message.ToolCalls[0].ArgumentsJson = []byte(`{"zip":94110}`)
	}
	cases := []struct {
		name       string
		curSig     string
		repSig     string
		contentChg bool
		wantReject bool
		wantClass  string
	}{
		{"intact pass-through", "tok_abc", "tok_abc", false, false, ""},
		{"unchanged over changed content (host invalidates on apply)", "tok_abc", "tok_abc", true, false, ""},
		{"cleared with content change", "tok_abc", "", true, false, ""},
		{"dropped without content change", "tok_abc", "", false, true, "dropped"},
		{"forged: replaced with another token", "tok_abc", "tok_evil", true, true, "forged"},
		{"added: token minted where none existed", "", "tok_new", false, true, "added"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			current := proto.Clone(base).(*pbv2.ChatResponse)
			current.Message.ToolCalls[0].Signature = tc.curSig
			replacement := proto.Clone(current).(*pbv2.ChatResponse)
			replacement.Message.ToolCalls[0].Signature = tc.repSig
			if tc.contentChg {
				changeContent(replacement)
			}
			err := validateResponseReplacement(current, replacement)
			if tc.wantReject {
				if err == nil {
					t.Fatalf("%s must be rejected", tc.name)
				}
				if want := "tool call 0 signature " + tc.wantClass; !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name signature class %q", err, tc.wantClass)
				}
			} else if err != nil {
				t.Errorf("%s must be accepted: %v", tc.name, err)
			}
		})
	}
}

// CRITERION 2 (mixed): a fully legal mutation — in-place name+arguments
// rewrite with the bound signature cleared in response — passes every check:
// host-owned facts untouched, id kept, presence and cardinality fixed.
func TestValidateResponseReplacementMixedValidMutation(t *testing.T) {
	current := hostOwnedBase()
	replacement := proto.Clone(current).(*pbv2.ChatResponse)
	tc := replacement.Message.ToolCalls[0]
	tc.Name = "get_forecast"
	tc.ArgumentsJson = []byte(`{"zip":94110}`)
	tc.Signature = "" // cleared because the covered content changed
	if err := validateResponseReplacement(current, replacement); err != nil {
		t.Errorf("name+args mutation with cleared signature must pass: %v", err)
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
