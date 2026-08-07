package proxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

// --- Response apply boundary: the host writes only what the wire can prove ---

// CRITERION 8a — writable-slot-only. Content is applied only when the body has
// a writable text slot. A body with tool calls and NO content key must not
// gain one — even though the pipeline runs a mutation and the body is
// re-marshaled. test-mutator rewrites arguments from run_after_response and
// never touches content, so the rewritten args prove the pipeline ran while
// the still-absent content key proves the host did not fabricate a slot.
func TestResponseApplyDoesNotAddContentSlot(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-mutator"})

	body := `{"id":"chatcmpl-9","object":"chat.completion","model":"gpt-x",
		"choices":[{"index":0,"finish_reason":"tool_calls","message":{
			"role":"assistant",
			"tool_calls":[{"id":"call_1","type":"function","function":{"name":"write","arguments":` + jsonStr(`{"k":"v"}`) + `}}]
		}}]}`

	out, err := runJSONResponseHooks(responseHookContext(1), pp, 1, "openai", nil, []byte(body))
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, has := msg["content"]; has {
		t.Fatalf("body gained a content key it never had: %s", out)
	}
	tcs := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(tcs))
	}
	args := tcs[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if !strings.Contains(args, "mutated_by") {
		t.Fatalf("test-mutator did not rewrite the arguments, so this proves "+
			"nothing about content-slot handling: %s", out)
	}
}

// CRITERION 8b — positional apply, no slot add/remove. Two tool calls, both
// rewritten in place. The call ids are host-owned and survive; no third call
// appears, so the positional identity of "call 0 mutates call 0" holds.
func TestResponseApplyRewritesCallsPositionally(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-mutator"})

	body := `{"id":"chatcmpl-10","choices":[{"index":0,"message":{
		"role":"assistant","content":"hi",
		"tool_calls":[
			{"id":"call_a","type":"function","function":{"name":"a","arguments":` + jsonStr(`{"x":1}`) + `}},
			{"id":"call_b","type":"function","function":{"name":"b","arguments":` + jsonStr(`{"y":2}`) + `}}
		]}}]}`

	out, err := runJSONResponseHooks(responseHookContext(1), pp, 1, "openai", nil, []byte(body))
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	tcs := msg["tool_calls"].([]any)
	if len(tcs) != 2 {
		t.Fatalf("tool calls = %d, want 2 (no slot added or removed)", len(tcs))
	}
	for i, wantID := range []string{"call_a", "call_b"} {
		tc := tcs[i].(map[string]any)
		if tc["id"] != wantID {
			t.Errorf("call %d id = %v, want %q (host-owned id must survive)", i, tc["id"], wantID)
		}
		args, _ := tc["function"].(map[string]any)["arguments"].(string)
		if !strings.Contains(args, "mutated_by") {
			t.Errorf("call %d arguments not rewritten: %q", i, args)
		}
	}
}

// CRITERIA 8c + 7a + finding 2 — ID and Signature are host-owned: a
// replacement that forges them is REJECTED wholesale (never half-applied, and
// never chained to a downstream plugin). test-forge-response-fields sets
// exactly those two fields, so under allow mode the pipeline refuses the
// replacement and the original bytes come back verbatim; under a block
// override the refusal surfaces as an error. The gemini half doubles as 7a:
// an untouched signed call keeps its token.
func TestResponseApplyRejectsForgedGuestFields(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-forge-response-fields/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-forge-response-fields"})

	forgeBlockPipeline := func(t *testing.T) *plugin.PluginPipeline {
		t.Helper()
		rt := wasm.NewRuntime(context.Background())
		t.Cleanup(func() { _ = rt.Close() })
		digest, err := plugin.BundleDigestForDir(fixturesDir + "/test-forge-response-fields")
		if err != nil {
			t.Fatal(err)
		}
		pp, err := plugin.NewPipeline(rt, plugin.PluginConfig{
			Dir:   fixturesDir,
			Order: []string{"test-forge-response-fields"},
			Approvals: map[string]plugin.Approval{
				"torana-test/test-forge-response-fields": {Digest: digest, Permissions: []string{"env.log"}, FailureMode: "block"},
			},
		})
		if err != nil {
			t.Fatalf("NewPipeline: %v", err)
		}
		return pp
	}

	// Each subtest is ONE response (one request on the real host), and the
	// host tracks stream topology per REQUEST — indexes stay unique within a
	// request, so the allow subtests give each response its own request ID.
	for i, tc := range []struct {
		name   string
		format string
		body   string
	}{
		{
			name:   "openai call id",
			format: "openai",
			body: `{"id":"chatcmpl-11","choices":[{"index":0,"message":{
				"role":"assistant",
				"tool_calls":[{"id":"call_orig","type":"function","function":{"name":"search","arguments":` + jsonStr(`{"q":"x"}`) + `}}]
			}}]}`,
		},
		{
			name:   "gemini thoughtSignature",
			format: "gemini",
			body: `{"candidates":[{"content":{"parts":[
				{"thoughtSignature":"tsig_orig","functionCall":{"id":"c1","name":"search","args":{"q":"x"}}}
			]}}]}`,
		},
	} {
		t.Run(tc.name+"/allow", func(t *testing.T) {
			out, err := runJSONResponseHooks(responseHookContext(uint64(i+1)), pp, uint64(i+1), tc.format, nil, []byte(tc.body))
			if err != nil {
				t.Fatalf("allow mode must not error: %v", err)
			}
			// The forged replacement was refused, so nothing changed on the
			// wire: the original bytes come back verbatim — the strongest form
			// of "forged id/signature was never written".
			if string(out) != tc.body {
				t.Fatalf("output changed, forged fields leaked or body re-marshaled:\n%s", out)
			}
		})

		t.Run(tc.name+"/block", func(t *testing.T) {
			bpp := forgeBlockPipeline(t)
			out, err := runJSONResponseHooks(responseHookContext(uint64(i+1)), bpp, uint64(i+1), tc.format, nil, []byte(tc.body))
			if err == nil {
				t.Fatal("block mode must return an error for a forged host-owned field")
			}
			if !strings.Contains(err.Error(), "test-forge-response-fields") {
				t.Errorf("error %q does not name the offending plugin", err)
			}
			if string(out) != tc.body {
				t.Fatalf("rejection was not atomic — output differs from the input body:\n%s", out)
			}
		})
	}
}

// Defensive apply boundary: a replacement that violates the host's relative
// policy must fail loudly — never a silent skip, never a half-applied body.
// test-invalid-replacement poisons content AND drops the last tool call; both
// violations must be refused atomically, with the original bytes untouched.
//
// The fixture's manifest is failure_mode pass, so the test pins the failure
// behaviour through an operator approval override — the same way the pipeline
// suite's block-mode test does. Under block mode the rejection surfaces as an
// error out of runJSONResponseHooks (propagated from RunAfterResponse) instead
// of a logged-and-dropped replacement.
func TestResponseApplyRejectsRelativeViolation(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-invalid-replacement/plugin.wasm")

	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { _ = rt.Close() })
	digest, err := plugin.BundleDigestForDir(fixturesDir + "/test-invalid-replacement")
	if err != nil {
		t.Fatal(err)
	}
	pp, err := plugin.NewPipeline(rt, plugin.PluginConfig{
		Dir:   fixturesDir,
		Order: []string{"test-invalid-replacement"},
		Approvals: map[string]plugin.Approval{
			"torana-test/test-invalid-replacement": {Digest: digest, FailureMode: "block"},
		},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if pp.Len() != 1 {
		t.Fatalf("loaded %d plugins, want 1", pp.Len())
	}

	body := `{"id":"chatcmpl-12","choices":[{"index":0,"message":{
		"role":"assistant","content":"hi",
		"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"a","arguments":` + jsonStr(`{"x":1}`) + `}},
			{"id":"call_2","type":"function","function":{"name":"b","arguments":` + jsonStr(`{"y":2}`) + `}}
		]}}]}`

	out, err := runJSONResponseHooks(responseHookContext(1), pp, 1, "openai", nil, []byte(body))
	if err == nil {
		t.Fatalf("a relative-policy-violating replacement was accepted")
	}
	if string(out) != body {
		t.Fatalf("rejection was not atomic — output differs from the input body:\n%s", out)
	}
	if strings.Contains(string(out), "poisoned-content") {
		t.Fatalf("poisoned content reached the body: %s", out)
	}
}
