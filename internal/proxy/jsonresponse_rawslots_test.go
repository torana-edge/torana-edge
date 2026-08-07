package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Round-10 finding 2 [high]: unselected alternatives' raw args corrupted.
//
// Mutable/exposed tool calls are the SELECTED response only (choice 0 /
// candidate 0). But the byte-restore pass must cover EVERY provider-body
// tool-argument slot: an unselected alternative is still re-encoded by the
// marshaled-map round-trip (sorted object keys, big integers rounded through
// float64) even though the pipeline never touched it. rawSlots records each
// slot; slots whose call is -1 are restored verbatim.
// ---------------------------------------------------------------------------

func TestRawPreservationUnselectedCandidateGemini(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-mutator"})

	// Candidate 1 carries a NONCANONICAL args object (odd whitespace + an
	// integer above 2^53) and its own thoughtSignature. test-mutator rewrites
	// only candidate 0's arguments.
	body := []byte(`{"candidates":[
		{"content":{"parts":[{"functionCall":{"id":"c1","name":"a","args":{"x":1}}}]}},
		{"content":{"parts":[{"thoughtSignature":"sig2","functionCall":{"id":"c2","name":"b","args":{"zzz": 1, "aaa": 9007199254740993 }}}]}}
	]}`)

	out, err := runJSONResponseHooks(responseHookContext(1), pp, 1, "gemini", nil, body)
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}
	// Candidate 1's args must be BYTE-IDENTICAL to the input: exact whitespace
	// and the un-rounded big int.
	if !strings.Contains(string(out), `"args":{"zzz": 1, "aaa": 9007199254740993 }`) {
		t.Fatalf("unselected candidate args were re-encoded:\n%s", out)
	}
	if strings.Contains(string(out), "9007199254740992") {
		t.Fatalf("the big int leaked through a lossy float64 round-trip:\n%s", out)
	}
	// Candidate 1's signature survives — only selected calls are mutable, and
	// this one was never touched.
	if !strings.Contains(string(out), "sig2") {
		t.Fatalf("the unselected candidate's thoughtSignature was dropped:\n%s", out)
	}
	// Candidate 0 (the selected response) is still rewritten by the plugin.
	if !strings.Contains(string(out), `"args":{"mutated_by":"test-mutator"}`) {
		t.Fatalf("candidate 0 args were not rewritten:\n%s", out)
	}
}

func TestRawPreservationUnselectedChoiceOpenai(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-mutator"})

	// Choice 1's arguments is a JSON STRING slot; its bytes must survive a
	// choice-0 mutation verbatim. Odd spacing round-trips through the map, but
	// a raw splice must never disturb it either — and a `\/` escape does NOT
	// round-trip through json.Marshal, so only the verbatim splice can
	// preserve it.
	// Each subtest is ONE response (one request on the real host), and the
	// host tracks stream topology per REQUEST — indexes stay unique within a
	// request, so each response gets its own request ID.
	for i, tc := range []struct {
		name  string
		args1 string // raw wire bytes of choice 1's arguments string slot
	}{
		{"odd spacing", `"{\"y\": 2 }"`},
		{"escaped slash", `"{\"y\": \/2 }"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"id":"x","choices":[
				{"index":0,"message":{"role":"assistant","content":"A","tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"a","arguments":` + jsonStr(`{"x":1}`) + `}}]}},
				{"index":1,"message":{"role":"assistant","content":"B","tool_calls":[
					{"id":"call_2","type":"function","function":{"name":"b","arguments":` + tc.args1 + `}}]}}
			]}`

			out, err := runJSONResponseHooks(responseHookContext(uint64(i+1)), pp, uint64(i+1), "openai", nil, []byte(body))
			if err != nil {
				t.Fatalf("runJSONResponseHooks: %v", err)
			}
			if !strings.Contains(string(out), `"arguments":`+tc.args1) {
				t.Fatalf("choice 1 arguments were not byte-identical:\n%s", out)
			}
			if !strings.Contains(string(out), `"arguments":"{\"mutated_by\":\"test-mutator\"}"`) {
				t.Fatalf("choice 0 arguments were not rewritten:\n%s", out)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Round-10 finding 3 [medium]: absent args slot + unrelated mutation errors.
//
// A gemini functionCall with NO args key plus a content-only mutation used to
// fail with "args slot not found": the restore pass looked the slot up before
// checking whether there was anything to restore. An absent slot must be
// skipped before any span lookup; only a plugin that genuinely changed the
// arguments (which CREATES the slot via the setter) makes the lookup required.
// ---------------------------------------------------------------------------

func TestNoArgsSlotContentMutationOK(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-observer/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-observer"})

	// functionCall has no args key — there is no raw slot and nothing to
	// restore. The observer's content change must land without an error.
	body := []byte(`{"candidates":[{"content":{"parts":[
		{"text":"hi"},
		{"functionCall":{"id":"c1","name":"ping"}}
	]}}]}`)

	rs, ctx := observerRS()
	out, err := runJSONResponseHooks(ctx, pp, rs.ID, "gemini", nil, body)
	if err != nil {
		t.Fatalf("a content-only mutation failed on a call without an args slot: %v", err)
	}
	if !strings.Contains(string(out), "observed status=") {
		t.Fatalf("the content mutation did not land:\n%s", out)
	}
	if strings.Contains(string(out), `"args"`) {
		t.Fatalf("an args key was fabricated for a call that never had one:\n%s", out)
	}
}

func TestNoArgsSlotPluginCreatedArgs(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-mutator"})

	// test-mutator replaces the arguments of every call. The setter creates
	// the args slot in the decoded map, so the restore lookup is required and
	// the plugin's exact bytes must reach the wire as a valid provider-shaped
	// object slot.
	body := []byte(`{"candidates":[{"content":{"parts":[
		{"text":"hi"},
		{"functionCall":{"id":"c1","name":"ping"}}
	]}}]}`)

	out, err := runJSONResponseHooks(responseHookContext(1), pp, 1, "gemini", nil, body)
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	fc := got["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)[1].(map[string]any)["functionCall"].(map[string]any)
	args, ok := fc["args"].(map[string]any)
	if !ok || args["mutated_by"] != "test-mutator" {
		t.Fatalf("the plugin-created args slot is missing or wrong: %v", fc["args"])
	}
}

// ---------------------------------------------------------------------------
// Round-10 finding 4 [medium]: duplicate keys — raw takes first, decoder
// takes last.
//
// encoding/json keeps the LAST duplicate key, so the raw view (rawJSONSpan
// at extraction time AND restore time) must agree with the decoded view.
// spanAt now matches the last occurrence of an object key; the restored
// bytes are the value the decoder/plugin actually saw.
// ---------------------------------------------------------------------------

func TestDuplicateArgsKeysLastOccurrenceWins(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-observer/plugin.wasm")
	pp := newProxyTestPipeline(t, []string{"test-observer"})

	// "arguments" appears TWICE inside function; the decoder keeps the second.
	// A content-only mutation elsewhere must restore the LAST occurrence's
	// bytes — the value the decoder (and therefore the plugin) saw.
	body := `{"id":"x","choices":[{"index":0,"message":{
		"role":"assistant","content":"hi",
		"tool_calls":[{"id":"call_1","type":"function","function":{
			"name":"a",
			"arguments":"{\"first\":1}",
			"arguments":"{\"second\":2}"
		}}]
	}}]}`

	rs, ctx := observerRS()
	out, err := runJSONResponseHooks(ctx, pp, rs.ID, "openai", nil, []byte(body))
	if err != nil {
		t.Fatalf("runJSONResponseHooks: %v", err)
	}
	if !strings.Contains(string(out), `"arguments":"{\"second\":2}"`) {
		t.Fatalf("restored bytes are not the LAST duplicate key's value:\n%s", out)
	}
	if strings.Contains(string(out), `"first"`) {
		t.Fatalf("the first duplicate's bytes leaked into the output:\n%s", out)
	}
}
