package proxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/cache"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

// ---------------------------------------------------------------------------
// Extractor presence (finding 1): the content slot exists only when the wire
// actually has a writable string text key — absent and JSON null both mean NO
// slot, so a plugin can never fabricate content presence.
// ---------------------------------------------------------------------------

func TestExtractPresenceOpenAIContentSlot(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantSlot  bool
		wantValue string
		wantMsg   bool
		wantCalls int
	}{
		{
			name: "absent", body: `{"choices":[{"message":{"role":"assistant"}}]}`,
			wantSlot: false, wantMsg: true, wantCalls: 0,
		},
		{
			name: "null", body: `{"choices":[{"message":{"role":"assistant","content":null}}]}`,
			wantSlot: false, wantMsg: true, wantCalls: 0,
		},
		{
			name: "present-empty", body: `{"choices":[{"message":{"role":"assistant","content":""}}]}`,
			wantSlot: true, wantValue: "", wantMsg: true, wantCalls: 0,
		},
		{
			name: "present-nonempty", body: `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`,
			wantSlot: true, wantValue: "hi", wantMsg: true, wantCalls: 0,
		},
		{
			name: "content-number", body: `{"choices":[{"message":{"role":"assistant","content":42}}]}`,
			wantSlot: false, wantMsg: true, wantCalls: 0,
		},
		{
			name: "no-message", body: `{"choices":[{"index":0,"finish_reason":"stop"}]}`,
			wantSlot: false, wantMsg: false, wantCalls: 0,
		},
		{
			name: "empty-choices", body: `{"choices":[]}`,
			wantSlot: false, wantMsg: false, wantCalls: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refs := extractOpenAI(decode(t, tc.body), []byte(tc.body))
			if (refs.setContent != nil) != tc.wantSlot {
				t.Fatalf("setContent slot present = %v, want %v", refs.setContent != nil, tc.wantSlot)
			}
			if refs.setContent != nil && refs.content != tc.wantValue {
				t.Errorf("content = %q, want %q", refs.content, tc.wantValue)
			}
			if refs.hasMessage != tc.wantMsg {
				t.Errorf("hasMessage = %v, want %v", refs.hasMessage, tc.wantMsg)
			}
			if len(refs.toolCalls) != tc.wantCalls {
				t.Errorf("tool calls = %d, want %d", len(refs.toolCalls), tc.wantCalls)
			}
		})
	}
}

// hasMessage is the per-format gate: the hook gets Message=nil unless the
// provider response actually contains an assistant turn.
func TestExtractPresenceHasMessagePerFormat(t *testing.T) {
	cases := []struct {
		format string
		body   string
		want   bool
	}{
		{"openai", `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`, true},
		{"openai", `{"choices":[{"finish_reason":"stop"}]}`, false},
		{"openai", `{"output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`, true},
		{"openai", `{"output":[]}`, false},
		{"anthropic", `{"content":[{"type":"text","text":"hi"}]}`, true},
		{"anthropic", `{"content":[]}`, false},
		{"anthropic", `{"stop_reason":"end_turn"}`, false},
		{"bedrock", `{"output":{"message":{"content":[{"text":"hi"}]}}}`, true},
		{"bedrock", `{"output":{"message":null}}`, false},
		{"gemini", `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`, true},
		{"gemini", `{"candidates":[]}`, false},
		{"gemini", `{"candidates":[{"finishReason":"STOP"}]}`, false},
		{"gemini-codeassist", `{"response":{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}}`, true},
		{"gemini-codeassist", `{"response":{"candidates":[]}}`, false},
		{"unknown-format", `{"choices":[{"message":{"content":"hi"}}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.format+"/"+tc.body, func(t *testing.T) {
			refs := extractResponse(tc.format, decode(t, tc.body), []byte(tc.body))
			if refs.hasMessage != tc.want {
				t.Errorf("hasMessage = %v, want %v", refs.hasMessage, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Finding 1, hook level: test-invented-content invents content whenever a
// message exists. On a slotless body that is a presence violation (rejected —
// the output must stay byte-identical); on a present-empty body it is a legal
// value change.
// ---------------------------------------------------------------------------

func TestSlotContentNotInventedWhenAbsent(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-invented-content/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-invented-content"})

	// Tool-call-only body: choices[0].message exists (hasMessage) but has NO
	// content key — there is no writable text slot to mutate.
	body := `{"id":"chatcmpl-1","choices":[{"index":0,"finish_reason":"tool_calls","message":{
		"role":"assistant",
		"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":` + jsonStr(`{"q":"x"}`) + `}}]
	}}]}`

	out, err := runJSONResponseHooks(context.Background(), pp, 1, "openai", nil, []byte(body))
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}
	// The invented-content replacement is a presence violation and is rejected
	// atomically — the strongest form of "no content key was fabricated" is
	// that the original bytes come back untouched.
	if string(out) != body {
		t.Fatalf("a content key was invented or the body re-marshaled:\n%s", out)
	}
}

func TestSlotInventedRejectedBlock(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-invented-content/plugin.wasm")

	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { _ = rt.Close() })
	digest, err := plugin.BundleDigestForDir(fixturesDir + "/test-invented-content")
	if err != nil {
		t.Fatal(err)
	}
	pp, err := plugin.NewPipeline(rt, plugin.PluginConfig{
		Dir:   fixturesDir,
		Order: []string{"test-invented-content"},
		Approvals: map[string]plugin.Approval{
			"torana-test/test-invented-content": {Digest: digest, FailureMode: "block"},
		},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	body := `{"id":"chatcmpl-2","choices":[{"index":0,"message":{
		"role":"assistant",
		"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":` + jsonStr(`{"q":"x"}`) + `}}]
	}}]}`

	out, err := runJSONResponseHooks(context.Background(), pp, 1, "openai", nil, []byte(body))
	if err == nil {
		t.Fatalf("a presence-violating replacement was accepted under block mode")
	}
	if string(out) != body {
		t.Fatalf("rejection was not atomic — output differs from the input body:\n%s", out)
	}
}

func TestSlotPresentEmptyInvented(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-invented-content/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-invented-content"})

	// content:"" is a PRESENT string key, so the slot exists and the value
	// change hi->invented is legal.
	body := `{"id":"chatcmpl-3","choices":[{"index":0,"message":{"role":"assistant","content":""}}]}`

	out, err := runJSONResponseHooks(context.Background(), pp, 1, "openai", nil, []byte(body))
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "invented" {
		t.Fatalf("content = %v, want %q (the present-empty slot was not rewritten)", msg["content"], "invented")
	}
}

// ---------------------------------------------------------------------------
// Finding 1, hook level: a message-less body (no candidates) must reach the
// hook with Message=nil — the observer's nil branch caches the upstream status
// through the host cache, proving the hook saw no assistant turn, and the body
// passes through unchanged.
// ---------------------------------------------------------------------------

func TestObserverNoCandidateGemini(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-observer/plugin.wasm")

	store := cache.NewLocalCache(time.Minute)
	t.Cleanup(store.Close)
	rt := wasm.NewRuntimeWithCache(context.Background(), store)
	t.Cleanup(func() { _ = rt.Close() })
	pp, err := plugin.NewPipeline(rt, plugin.PluginConfig{
		Dir: fixturesDir, Order: []string{"test-observer"}, AllowUnapproved: true,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	rs := &reqState{ID: 1, UpstreamStatus: 200}
	ctx := context.WithValue(context.Background(), reqStateKey{}, rs)
	body := []byte(`{"candidates":[]}`)

	out, err := runJSONResponseHooks(ctx, pp, rs.ID, "gemini", nil, body)
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}
	if string(out) != string(body) {
		t.Fatalf("a message-less body was rewritten:\n%s", out)
	}
	// The observer caches the status ONLY from its Message==nil branch, so a
	// hit proves run_after_response received ChatResponse{Message: nil}.
	v, ok := store.Get(observerCacheKey("observed_error_status"))
	if !ok || v != "200" {
		t.Fatalf("observer's Message==nil branch did not run: cache %q, present %v", v, ok)
	}
}

// ---------------------------------------------------------------------------
// Finding 3, hook level: raw argument spans survive the pipeline byte for
// byte. Re-marshaling the decoded map would sort keys and round 9007199254740993
// through float64 to 9007199254740992; the splice restores the provider's
// bytes while content mutations still land.
// ---------------------------------------------------------------------------

func observerRS() (*reqState, context.Context) {
	rs := &reqState{ID: 1, UpstreamStatus: 200, UsageIn: 7, UsageOut: 3}
	return rs, context.WithValue(context.Background(), reqStateKey{}, rs)
}

func TestRawPreservationGeminiArgs(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-observer/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-observer"})

	body := []byte(`{"candidates":[{"content":{"parts":[
		{"text":"hi"},
		{"thoughtSignature":"tsig","functionCall":{"id":"c1","name":"search","args":{"zzz":1,"aaa":9007199254740993}}}
	]}}]}`)

	rs, ctx := observerRS()
	out, err := runJSONResponseHooks(ctx, pp, rs.ID, "gemini", nil, body)
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}
	if !strings.Contains(string(out), `"args":{"zzz":1,"aaa":9007199254740993}`) {
		t.Fatalf("gemini args were not preserved byte for byte:\n%s", out)
	}
	if strings.Contains(string(out), "9007199254740992") {
		t.Fatalf("the big int leaked through a lossy float64 round-trip:\n%s", out)
	}
	if !strings.Contains(string(out), "tsig") {
		t.Fatalf("the untouched thoughtSignature was dropped:\n%s", out)
	}
	if !strings.Contains(string(out), "observed status=200 in=7 out=3") {
		t.Fatalf("the content mutation did not land:\n%s", out)
	}
}

// The codeassist wrapper nests the GenerateContentResponse under "response";
// the splice paths must carry that prefix or the args slot is never found in
// the marshaled output.
func TestRawPreservationGeminiCodeAssist(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-observer/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-observer"})

	body := []byte(`{"response":{"candidates":[{"content":{"parts":[
		{"text":"hi"},
		{"thoughtSignature":"tsig","functionCall":{"id":"c1","name":"search","args":{"zzz":1,"aaa":9007199254740993}}}
	]}}]}}`)

	rs, ctx := observerRS()
	out, err := runJSONResponseHooks(ctx, pp, rs.ID, "gemini-codeassist", nil, body)
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}
	if !strings.Contains(string(out), `"args":{"zzz":1,"aaa":9007199254740993}`) {
		t.Fatalf("codeassist args were not preserved byte for byte:\n%s", out)
	}
	if strings.Contains(string(out), "9007199254740992") {
		t.Fatalf("the big int leaked through a lossy float64 round-trip:\n%s", out)
	}
	if !strings.Contains(string(out), "tsig") {
		t.Fatalf("the untouched thoughtSignature was dropped:\n%s", out)
	}
	if !strings.Contains(string(out), "observed status=200 in=7 out=3") {
		t.Fatalf("the content mutation did not land:\n%s", out)
	}
}

func TestRawPreservationAnthropicInput(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-observer/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-observer"})

	body := []byte(`{"id":"msg_1","content":[
		{"type":"text","text":"hi"},
		{"type":"tool_use","id":"toolu_1","name":"search","input":{"zzz":1,"aaa":9007199254740993}}
	]}`)

	rs, ctx := observerRS()
	out, err := runJSONResponseHooks(ctx, pp, rs.ID, "anthropic", nil, body)
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}
	if !strings.Contains(string(out), `"input":{"zzz":1,"aaa":9007199254740993}`) {
		t.Fatalf("anthropic input was not preserved byte for byte:\n%s", out)
	}
	if strings.Contains(string(out), "9007199254740992") {
		t.Fatalf("the big int leaked through a lossy float64 round-trip:\n%s", out)
	}
	if !strings.Contains(string(out), "observed status=200 in=7 out=3") {
		t.Fatalf("the content mutation did not land:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Finding 4, hook level: a second choice/candidate is an ALTERNATIVE, not
// another turn — only the first is mutated; the alternative's bytes survive.
// ---------------------------------------------------------------------------

// The Responses API carries arguments as a JSON STRING; a rewritten call must
// stay a string on the wire (the map already holds the new text — no splice),
// and an untouched call keeps its verbatim quoted bytes.
func TestSlotResponsesArgumentsString(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-mutator"})

	body := `{"id":"resp_1","object":"response","model":"m","output":[
		{"type":"message","content":[{"type":"output_text","text":"hi"}]},
		{"type":"function_call","call_id":"call_1","name":"read","arguments":` + jsonStr(`{"path":"server.go"}`) + `}
	]}`

	out, err := runJSONResponseHooks(context.Background(), pp, 1, "openai", nil, []byte(body))
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	item := got["output"].([]any)[1].(map[string]any)
	if item["arguments"] != `{"mutated_by":"test-mutator"}` {
		t.Fatalf("rewritten arguments = %v, want the mutator's text", item["arguments"])
	}
	// The rewritten slot is a plain string value on the wire, not a nested
	// object.
	if _, isStr := item["arguments"].(string); !isStr {
		t.Fatalf("arguments are not a JSON string after the rewrite: %s", out)
	}
}

func TestChoice0OnlyOpenai(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-mutator"})

	body := `{"id":"x","choices":[
		{"index":0,"message":{"role":"assistant","content":"A","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"a","arguments":` + jsonStr(`{"x":1}`) + `}}]}},
		{"index":1,"message":{"role":"assistant","content":"B","tool_calls":[
			{"id":"call_2","type":"function","function":{"name":"b","arguments":` + jsonStr(`{"y":2}`) + `}}]}}
	]}`

	out, err := runJSONResponseHooks(context.Background(), pp, 1, "openai", nil, []byte(body))
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	choices := got["choices"].([]any)
	c0 := choices[0].(map[string]any)["message"].(map[string]any)
	c1 := choices[1].(map[string]any)["message"].(map[string]any)

	args0 := c0["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if args0 != `{"mutated_by":"test-mutator"}` {
		t.Fatalf("choice 0 arguments = %q, want the mutator's replacement", args0)
	}
	args1 := c1["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if args1 != `{"y":2}` {
		t.Fatalf("choice 1 arguments were rewritten: %q", args1)
	}
	if c1["content"] != "B" {
		t.Fatalf("choice 1 content was touched: %v", c1["content"])
	}
	if c1["tool_calls"].([]any)[0].(map[string]any)["id"] != "call_2" {
		t.Fatalf("choice 1 call id was touched: %v", c1["tool_calls"].([]any)[0].(map[string]any)["id"])
	}
}

func TestCandidate0OnlyGemini(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-mutator"})

	body := []byte(`{"candidates":[
		{"content":{"parts":[{"functionCall":{"id":"c1","name":"a","args":{"x":1}}}]}},
		{"content":{"parts":[{"functionCall":{"id":"c2","name":"b","args":{"y":2}}}]}}
	]}`)

	out, err := runJSONResponseHooks(context.Background(), pp, 1, "gemini", nil, body)
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	cands := got["candidates"].([]any)
	fc0 := cands[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
	fc1 := cands[1].(map[string]any)["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)

	if fc0["id"] != "c1" || fc1["id"] != "c2" {
		t.Fatalf("candidate ids changed: %v / %v", fc0["id"], fc1["id"])
	}
	if args, ok := fc0["args"].(map[string]any); !ok || args["mutated_by"] != "test-mutator" {
		t.Fatalf("candidate 0 args not rewritten: %v", fc0["args"])
	}
	if args, ok := fc1["args"].(map[string]any); !ok || args["y"] != float64(2) || len(args) != 1 {
		t.Fatalf("candidate 1 args were rewritten: %v", fc1["args"])
	}
	if !strings.Contains(string(out), `"args":{"y":2}`) {
		t.Fatalf("candidate 1 args not byte-identical in the output:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Round 11 finding 1: the gemini CONTENT slot must come from candidate 0 —
// the selected response — exactly like tool calls. A later candidate is an
// alternative, so its text must be neither exposed as ResponseMessage.content
// nor mutated on the wire. rawSlots still cover every candidate (args byte
// preservation is unaffected).
// ---------------------------------------------------------------------------

func TestGeminiContentSlotCandidateZeroOnly(t *testing.T) {
	// Candidate 0 has only a tool call; candidate 1 has text. The text must
	// NOT become the writable slot.
	body := `{"candidates":[
		{"content":{"parts":[{"functionCall":{"id":"c1","name":"ping"}}]}},
		{"content":{"parts":[{"text":"alternative"}]}}
	]}`
	refs := extractResponse("gemini", decode(t, body), []byte(body))
	if !refs.hasMessage {
		t.Fatal("candidate 0 with content must count as a message")
	}
	if refs.setContent != nil {
		t.Fatalf("content slot bound from the UNSELECTED candidate: %q", refs.content)
	}
	if refs.content != "" {
		t.Fatalf("unselected candidate text leaked into content: %q", refs.content)
	}

	// Present-empty text on candidate 0 IS the selected writable slot, even
	// when candidate 1 carries real text.
	body = `{"candidates":[
		{"content":{"parts":[{"text":""}]}},
		{"content":{"parts":[{"text":"alternative"}]}}
	]}`
	refs = extractResponse("gemini", decode(t, body), []byte(body))
	if refs.setContent == nil {
		t.Fatal("candidate-0 present-empty text must bind the writable slot")
	}
	if refs.content != "" {
		t.Fatalf("content = %q, want present-empty from candidate 0", refs.content)
	}
}

// Hook level: a content-mutating plugin must neither see candidate-1 text as
// ResponseMessage.content nor change candidate 1 on the wire.
func TestGeminiAlternativeTextNotMutated(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-observer/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-observer"})

	body := []byte(`{"candidates":[
		{"content":{"parts":[{"functionCall":{"id":"c1","name":"ping","args":{"x":1}}}]}},
		{"content":{"parts":[{"text":"alternative"}]}}
	]}`)

	out, err := runJSONResponseHooks(context.Background(), pp, 1, "gemini", nil, body)
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}
	if strings.Contains(string(out), "observed status=") {
		t.Fatalf("observer rewrote the unselected candidate's text:\n%s", out)
	}
	if !strings.Contains(string(out), `"text":"alternative"`) {
		t.Fatalf("candidate 1 text changed on the wire:\n%s", out)
	}
}

// Present-empty text on candidate 0 stays the selected slot end to end: the
// plugin's value change lands there, and candidate 1 is untouched.
func TestGeminiPresentEmptyTextIsSelectedSlot(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-invented-content/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-invented-content"})

	body := []byte(`{"candidates":[
		{"content":{"parts":[{"text":""}]}},
		{"content":{"parts":[{"text":"alternative"}]}}
	]}`)

	out, err := runJSONResponseHooks(context.Background(), pp, 1, "gemini", nil, body)
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}
	if !strings.Contains(string(out), `"text":"invented"`) {
		t.Fatalf("candidate-0 present-empty slot not rewritten:\n%s", out)
	}
	if !strings.Contains(string(out), `"text":"alternative"`) {
		t.Fatalf("candidate 1 text changed on the wire:\n%s", out)
	}
}
