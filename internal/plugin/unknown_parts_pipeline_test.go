package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format/gemini"
	"github.com/torana-edge/torana-edge/internal/format/openai"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// geminiAdapter is the adapter the final-wire assertions marshal through.
var geminiAdapter = &gemini.Adapter{}
var openaiAdapter = &openai.Adapter{}

// The unknown-part contract through REAL guests (review finding 3): a
// provider-valid unmodelled part survives a WASM pass and a WASM
// replacement byte-exactly, at its wire position.

func unknownPartRequest() *engine.ChatRequest {
	return &engine.ChatRequest{
		Model: "gemini",
		Messages: []engine.Message{{
			Role: engine.RoleUser,
			Blocks: []engine.Block{
				{Text: &engine.TextBlock{Text: "look at this"}},
				{Unknown: &engine.UnknownBlock{Kind: "part", Payload: mustReqForTest(`{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}}`)}},
				{Text: &engine.TextBlock{Text: "and this"}},
			},
		}},
	}
}

// TestUnknownPartsSurviveWASMPass — test-inert-a leaves the request alone;
// the unknown arm must come back byte-identical.
func TestUnknownPartsSurviveWASMPass(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-inert-a/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-inert-a"})

	chat := unknownPartRequest()
	out, err := pp.RunBeforeRequest(context.Background(), 7, chat, nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	if out == nil {
		t.Fatal("pass-through returned nothing")
	}
	if len(out.Messages) != 1 || len(out.Messages[0].Blocks) != 3 {
		t.Fatalf("blocks changed through the pass: %+v", out.Messages[0].Blocks)
	}
	u := out.Messages[0].Blocks[1].Unknown
	if u == nil || string(u.Payload.Bytes()) != `{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}}` {
		t.Fatalf("unknown arm changed through the pass: %+v", out.Messages[0].Blocks[1])
	}
}

// TestUnknownPartsSurviveWASMReplacement — test-mutator rewrites the user
// text; the unknown arm must survive the replacement byte-exactly.
func TestUnknownPartsSurviveWASMReplacement(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-mutator"})

	chat := unknownPartRequest()
	out, err := pp.RunBeforeRequest(context.Background(), 8, chat, nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	if out == nil {
		t.Fatal("replacement returned nothing")
	}
	if len(out.Messages) != 1 || len(out.Messages[0].Blocks) != 3 {
		t.Fatalf("blocks changed through the replacement: %+v", out.Messages[0].Blocks)
	}
	u := out.Messages[0].Blocks[1].Unknown
	if u == nil || string(u.Payload.Bytes()) != `{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}}` {
		t.Fatalf("unknown arm changed through the replacement: %+v", out.Messages[0].Blocks[1])
	}
}

// signedMediaPartRequest carries a SIGNED media part — the REV 4 carriers:
// thoughtSignature on the unknown arm, partMetadata on the part, and a
// signed tool-result block with presence-aware will_continue/scheduling.
func signedMediaPartRequest() *engine.ChatRequest {
	pm := mustOptReqForTest(`{"src":"gcs://bucket/raw.png"}`)
	meta := mustOptReqForTest(`{"team":"x"}`)
	wc := false
	sched := "SILENT"
	return &engine.ChatRequest{
		Model: "gemini",
		Messages: []engine.Message{{
			Role: engine.RoleUser,
			Blocks: []engine.Block{
				{Text: &engine.TextBlock{Text: "look at this"}},
				{Unknown: &engine.UnknownBlock{
					Kind:             "part",
					Payload:          mustReqForTest(`{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}}`),
					PartMetadataJson: pm,
					Signature:        "MEDIA_SIG",
				}},
			},
		}, {
			Role: engine.RoleTool,
			Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{
				ToolCallID:       "c1",
				ToolName:         "read",
				PartMetadataJson: meta,
				WillContinue:     &wc,
				Scheduling:       &sched,
				Signature:        "RESP_SIG",
				Content: []engine.ToolResultContentBlock{
					{Text: `{"output":"out"}`},
					{Unknown: &engine.UnknownBlock{Kind: "part", Payload: mustReqForTest(`{"inlineData":{"mimeType":"image/png","data":"iVBOR"}}`)}},
				},
			}}},
		}},
	}
}

// TestSignedPartsSurviveWASMPass — the typed signed-Part carriers (unknown
// signature, part metadata, tool-result signature with presence-aware
// will_continue/scheduling and the nested media element) survive a real
// WASM pass byte-exactly, in order.
func TestSignedPartsSurviveWASMPass(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-inert-a/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-inert-a"})

	chat := signedMediaPartRequest()
	out, err := pp.RunBeforeRequest(context.Background(), 9, chat, nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	if out == nil {
		t.Fatal("pass-through returned nothing")
	}
	if len(out.Messages) != 2 {
		t.Fatalf("messages changed through the pass: %+v", out.Messages)
	}
	u := out.Messages[0].Blocks[1].Unknown
	if u == nil || u.Signature != "MEDIA_SIG" || string(u.PartMetadataJson.Bytes()) != `{"src":"gcs://bucket/raw.png"}` {
		t.Fatalf("signed media carriers changed through the pass: %+v", out.Messages[0].Blocks[1])
	}
	tr := out.Messages[1].Blocks[0].ToolResult
	if tr == nil || tr.Signature != "RESP_SIG" || string(tr.PartMetadataJson.Bytes()) != `{"team":"x"}` {
		t.Fatalf("tool-result carriers changed through the pass: %+v", out.Messages[1].Blocks[0])
	}
	if tr.WillContinue == nil || *tr.WillContinue {
		t.Fatalf("will_continue presence lost through the pass: %+v", tr.WillContinue)
	}
	if tr.Scheduling == nil || *tr.Scheduling != "SILENT" {
		t.Fatalf("scheduling lost through the pass: %+v", tr.Scheduling)
	}
	if len(tr.Content) != 2 || tr.Content[1].Unknown == nil {
		t.Fatalf("nested media element lost through the pass: %+v", tr.Content)
	}
}

// TestSignedPartsSurviveWASMReplacement — test-mutator rewrites the user
// text; the signed-Part carriers must survive the replacement exactly.
func TestSignedPartsSurviveWASMReplacement(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-mutator"})

	chat := signedMediaPartRequest()
	out, err := pp.RunBeforeRequest(context.Background(), 10, chat, nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	if out == nil {
		t.Fatal("replacement returned nothing")
	}
	u := out.Messages[0].Blocks[1].Unknown
	if u == nil || u.Signature != "MEDIA_SIG" || string(u.PartMetadataJson.Bytes()) != `{"src":"gcs://bucket/raw.png"}` {
		t.Fatalf("signed media carriers changed through the replacement: %+v", out.Messages[0].Blocks[1])
	}
	tr := out.Messages[1].Blocks[0].ToolResult
	if tr == nil || tr.Signature != "RESP_SIG" {
		t.Fatalf("tool-result signature changed through the replacement: %+v", out.Messages[1].Blocks[0])
	}
}

// mustOptReqForTest parses a strict object into the optional carrier.
func mustOptReqForTest(raw string) engine.OptionalJSONObject {
	obj, err := engine.ParseOptionalJSONObject([]byte(raw))
	if err != nil {
		panic(err)
	}
	return obj
}

// frpRequest carries a tool result with ONE FunctionResponsePart (the
// exact one-arm wrapper as the nested Unknown payload).
func frpRequest(arm, inner string) *engine.ChatRequest {
	return &engine.ChatRequest{
		Model: "gemini",
		Messages: []engine.Message{{Role: engine.RoleTool, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{
			ToolCallID: "c1",
			ToolName:   "read",
			Content: []engine.ToolResultContentBlock{
				{Text: `{"output":"x"}`},
				{Unknown: &engine.UnknownBlock{Kind: "part", Payload: mustReqForTest(`{` + arm + `:{` + inner + `}}`)}},
			},
		}}}}},
	}
}

// TestFunctionResponsePartSurvivesWASMPassAndReplacement — the exact
// one-arm WRAPPER payload survives a real WASM pass AND a replacement
// byte-exactly (both arms incl. displayName), and the returned engine
// request marshals through the gemini adapter to the final
// functionResponse.parts structure.
func TestFunctionResponsePartSurvivesWASMPassAndReplacement(t *testing.T) {
	arms := []struct{ arm, inner string }{
		{`"inlineData"`, `"mimeType":"image/png","data":"iVBOR","displayName":"pic.png"`},
		{`"fileData"`, `"mimeType":"video/mp4","fileUri":"gs://b/x.mp4","displayName":"clip"`},
	}
	for _, a := range arms {
		t.Run(a.arm, func(t *testing.T) {
			want := `{` + a.arm + `:{` + a.inner + `}}`

			// Pass (test-inert-a).
			requireWASM(t, fixturesDir+"/test-inert-a/plugin.wasm")
			pp := newTestPipeline(t, fixturesDir, []string{"test-inert-a"})
			chat := frpRequest(a.arm, a.inner)
			out, err := pp.RunBeforeRequest(context.Background(), 11, chat, nil)
			if err != nil {
				t.Fatalf("pass: %v", err)
			}
			if got := string(out.Messages[0].Blocks[0].ToolResult.Content[1].Unknown.Payload.Bytes()); got != want {
				t.Fatalf("pass changed the wrapper payload:\n got %s\nwant %s", got, want)
			}
			wire, err := geminiAdapter.Marshal(out)
			if err != nil {
				t.Fatalf("pass: marshal: %v", err)
			}
			if err := assertFRPWire(t, wire, a.arm, a.inner); err != nil {
				t.Fatalf("pass: %v", err)
			}

			// Replacement (test-mutator rewrites the user text).
			requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
			pp2 := newTestPipeline(t, fixturesDir, []string{"test-mutator"})
			chat2 := frpRequest(a.arm, a.inner)
			out2, err := pp2.RunBeforeRequest(context.Background(), 12, chat2, nil)
			if err != nil {
				t.Fatalf("replacement: %v", err)
			}
			if got := string(out2.Messages[0].Blocks[0].ToolResult.Content[1].Unknown.Payload.Bytes()); got != want {
				t.Fatalf("replacement changed the wrapper payload:\n got %s\nwant %s", got, want)
			}
			wire2, err := geminiAdapter.Marshal(out2)
			if err != nil {
				t.Fatalf("replacement: marshal: %v", err)
			}
			if err := assertFRPWire(t, wire2, a.arm, a.inner); err != nil {
				t.Fatalf("replacement: %v", err)
			}
		})
	}
}

// assertFRPWire STRUCTURALLY decodes the final gemini wire and pins the
// exact functionResponse.parts shape: exactly one content with one part
// carrying a functionResponse, exactly one parts element with exactly the
// expected arm, and exact mimeType/data/fileUri/displayName values.
func assertFRPWire(t *testing.T, wire []byte, arm, inner string) error {
	t.Helper()
	arm = strings.Trim(arm, `"`) // the caller passes the JSON-quoted arm
	_ = inner
	var doc map[string]any
	if err := json.Unmarshal(wire, &doc); err != nil {
		return fmt.Errorf("wire is not JSON: %v", err)
	}
	contents, ok := doc["contents"].([]any)
	if !ok || len(contents) != 1 {
		return fmt.Errorf("want exactly one content, got %v", contents)
	}
	msg, ok := contents[0].(map[string]any)
	if !ok {
		return fmt.Errorf("content is not an object")
	}
	parts, ok := msg["parts"].([]any)
	if !ok || len(parts) != 1 {
		return fmt.Errorf("want exactly one part, got %v", parts)
	}
	part, ok := parts[0].(map[string]any)
	if !ok {
		return fmt.Errorf("part is not an object")
	}
	frRaw, ok := part["functionResponse"].(map[string]any)
	if !ok {
		return fmt.Errorf("no functionResponse on the part: %v", part)
	}
	frParts, ok := frRaw["parts"].([]any)
	if !ok || len(frParts) != 1 {
		return fmt.Errorf("want exactly one functionResponse.parts element, got %v", frRaw["parts"])
	}
	frp, ok := frParts[0].(map[string]any)
	if !ok {
		return fmt.Errorf("parts element is not an object")
	}
	if len(frp) != 1 {
		return fmt.Errorf("parts element has %d outer members, want exactly the one arm: %v", len(frp), frp)
	}
	armVal, ok := frp[arm]
	if !ok {
		return fmt.Errorf("parts element lacks arm %q: %v", arm, frp)
	}
	innerObj, ok := armVal.(map[string]any)
	if !ok {
		return fmt.Errorf("arm %q is not an object", arm)
	}
	var want map[string]string
	if arm == "inlineData" {
		want = map[string]string{"mimeType": "image/png", "data": "iVBOR", "displayName": "pic.png"}
	} else {
		want = map[string]string{"mimeType": "video/mp4", "fileUri": "gs://b/x.mp4", "displayName": "clip"}
	}
	if len(innerObj) != len(want) {
		return fmt.Errorf("arm %q has %d inner members, want exactly %d: %v", arm, len(innerObj), len(want), innerObj)
	}
	for k, v := range want {
		got, ok := innerObj[k].(string)
		if !ok || got != v {
			return fmt.Errorf("arm %q member %q = %v, want %q", arm, k, innerObj[k], v)
		}
	}
	return nil
}

// TestCodeAssistReplacementCanonicalOverlay — a REAL replacement
// (test-mutator rewrites the user text) with the approved canonical
// overlay: model/max_tokens/temperature/top_p/safety changes on the
// ACCEPTED request reach the exact final Gemini wire while the wrapper/
// inner extras survive at their exact scopes.
func TestCodeAssistReplacementCanonicalOverlay(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-mutator"})

	maxTok := 4096
	temp := 0.4
	topP := 0.8
	safety, _ := engine.ParseOptionalJSONArray([]byte(`[{"category":"HARM_CATEGORY_HARASSMENT","threshold":"BLOCK_NONE"}]`))
	env, _ := engine.ParseOptionalJSONObject([]byte(`{"project":"p-1","request":{"sessionId":"s-9"}}`))
	chat := &engine.ChatRequest{
		Model: "gemini-3.5-flash-extra-low", MaxTokens: &maxTok, Temperature: &temp, TopP: &topP,
		SafetySettings: safety, CodeAssist: true, ProviderExtensions: env,
		Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 14, chat, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages[0].Blocks[0].Text.Text == "hi" {
		t.Fatal("test-mutator did not replace (the pin is vacuous)")
	}
	wire, err := geminiAdapter.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(wire, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "gemini-3.5-flash-extra-low" {
		t.Fatalf("canonical model lost: %v", doc["model"])
	}
	if doc["project"] != "p-1" {
		t.Fatalf("wrapper extra lost: %v", doc["project"])
	}
	req := doc["request"].(map[string]any)
	if req["sessionId"] != "s-9" {
		t.Fatalf("inner extra lost: %v", req["sessionId"])
	}
	gc := req["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(4096) || gc["temperature"] != 0.4 || gc["topP"] != 0.8 {
		t.Fatalf("canonical generation members lost: %v", gc)
	}
	if _, ok := req["safetySettings"]; !ok {
		t.Fatalf("canonical safety lost: %v", req)
	}
}

// TestOpenAIResponsesReplacementLayout — a REAL replacement on a Responses
// request: the typed variant and the exact input layout survive and
// re-splice opaque items on the final Responses wire.
func TestOpenAIResponsesReplacementLayout(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-mutator"})

	layout, _ := engine.ParseOptionalJSONArray([]byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"reasoning","encrypted_content":"opaque-reasoning"}]`))
	chat := &engine.ChatRequest{
		Model: "gpt-5.4", OpenAIVariant: engine.OpenAIResponses, ResponsesInputLayout: layout,
		Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 15, chat, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages[0].Blocks[0].Text.Text == "hi" {
		t.Fatal("test-mutator did not replace (the pin is vacuous)")
	}
	if out.OpenAIVariant != engine.OpenAIResponses {
		t.Fatal("variant lost through the replacement")
	}
	if out.ResponsesInputLayout.IsAbsent() || string(out.ResponsesInputLayout.Bytes()) != string(layout.Bytes()) {
		t.Fatalf("layout lost through the replacement: %q", out.ResponsesInputLayout.Bytes())
	}
	wire, err := openaiAdapter.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	// STRICT final-input decode: exactly two items in the recorded order —
	// slot 0 the replaced representable message (with the MUTATED content),
	// slot 1 the opaque reasoning item with its exact payload; nothing
	// added, dropped, or moved.
	var doc map[string]any
	if err := json.Unmarshal(wire, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "gpt-5.4" {
		t.Fatalf("responses variant lost on the wire: %s", wire)
	}
	items, ok := doc["input"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("final input must have exactly 2 items, got %v", doc["input"])
	}
	slot0, ok := items[0].(map[string]any)
	if !ok || slot0["type"] != "message" || slot0["role"] != "user" {
		t.Fatalf("slot 0 is not the message item: %v", items[0])
	}
	ct, ok := slot0["content"].(string)
	if !ok || ct == "hi" || !strings.Contains(ct, "seen by test-mutator") {
		t.Fatalf("slot 0 does not carry the MUTATED content: %v", slot0["content"])
	}
	slot1, ok := items[1].(map[string]any)
	if !ok || slot1["type"] != "reasoning" || slot1["encrypted_content"] != "opaque-reasoning" {
		t.Fatalf("slot 1 is not the opaque reasoning item at its recorded position: %v", items[1])
	}
}

// TestCodeAssistSiblingCorpusThroughReplacement — the lexical sibling
// corpus passes through a REAL replacement and stays exact on the final
// wire.
func TestCodeAssistSiblingCorpusThroughReplacement(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-mutator"})

	env, _ := engine.ParseOptionalJSONObject([]byte(`{"request":{"generationConfig":{"z":1e999,"a":1.0,"w": 42 ,"nested":{"k":[1,2]}}}}`))
	chat := &engine.ChatRequest{
		Model: "m", CodeAssist: true, ProviderExtensions: env,
		Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 16, chat, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages[0].Blocks[0].Text.Text == "hi" {
		t.Fatal("test-mutator did not replace (the pin is vacuous)")
	}
	wire, err := geminiAdapter.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	preserved := `"generationConfig":{"z":1e999,"a":1.0,"w": 42 ,"nested":{"k":[1,2]}}`
	if !strings.Contains(string(wire), preserved) {
		t.Fatalf("sibling corpus not lexeme-exact after the replacement:\nwant %s\n got %s", preserved, wire)
	}
}

// smuggleValidator is the gemini Code Assist candidate validator used by
// the pipeline pins (mirrors the proxy wiring).
func smuggleValidator(topo engine.TopologyFacts, current, replacement *pb.ChatRequest) error {
	if topo.CodeAssist {
		return gemini.VerifyCodeAssistEnvelopePB(replacement.ProviderExtensionsJson)
	}
	return nil
}

// codeAssistRequest builds a Code Assist engine request with the typed
// flag and a valid envelope.
func codeAssistRequest() *engine.ChatRequest {
	env, err := engine.ParseOptionalJSONObject([]byte(`{"project":"p-1","request":{"sessionId":"s-9"}}`))
	if err != nil {
		panic(err)
	}
	return &engine.ChatRequest{
		Model:              "gemini-3.5-flash",
		CodeAssist:         true,
		ProviderExtensions: env,
		Messages:           []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}}},
	}
}

// TestEnvelopeSmugglingBlockMode — a real guest smuggling a canonical
// member into the Code Assist envelope is attributed to THAT plugin: in
// block mode the replacement is refused with the plugin named.
func TestEnvelopeSmugglingBlockMode(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-envelope-smuggler/plugin.wasm")
	pp := pipelineWithValidator(t, fixturesDir, []string{"test-envelope-smuggler"}, smuggleValidator)

	chat := codeAssistRequest()
	_, err := pp.RunBeforeRequest(context.Background(), 20, chat, nil)
	if err == nil {
		t.Fatal("block-mode smuggling must be refused")
	}
	if !strings.Contains(err.Error(), "test-envelope-smuggler") {
		t.Fatalf("error must name the plugin: %v", err)
	}
}

// TestEnvelopeSmugglingPassRollsBackAndChains — in pass mode the smuggled
// replacement is dropped and the PRE-PLUGIN request chains to an
// OBSERVABLE downstream guest (test-records-invocation tags the model):
// exactly ONE downstream invocation, ordered after the refused smuggler,
// with the BYTE-EXACT pre-smuggler envelope at that hook boundary.
func TestEnvelopeSmugglingPassRollsBackAndChains(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-envelope-smuggler/plugin.wasm")
	requireWASM(t, fixturesDir+"/test-records-invocation/plugin.wasm")

	smugDigest, err := BundleDigestForDir(fixturesDir + "/test-envelope-smuggler")
	if err != nil {
		t.Fatal(err)
	}
	recDigest, err := BundleDigestForDir(fixturesDir + "/test-records-invocation")
	if err != nil {
		t.Fatal(err)
	}
	pp := pipelineWithApproval(t, fixturesDir, []string{"test-envelope-smuggler", "test-records-invocation"}, map[string]provider.PluginApproval{
		"test-envelope-smuggler":  {Digest: smugDigest, Permissions: []string{"ir.params.write"}, FailureMode: "pass"},
		"test-records-invocation": {Digest: recDigest, Permissions: []string{"ir.model.write"}, FailureMode: "pass"},
	}, smuggleValidator)

	chat := codeAssistRequest()
	out, err := pp.RunBeforeRequest(context.Background(), 21, chat, nil)
	if err != nil {
		t.Fatalf("pass mode must not error: %v", err)
	}
	if out == nil {
		t.Fatal("no chained output")
	}
	// EXACTLY ONE downstream invocation, after the refused smuggler: the
	// model carries the invocation tag (the downstream ran on the chained
	// request), and the tag appears once.
	if out.Model != "gemini-3.5-flash+downstream-ran" {
		t.Fatalf("downstream did not observe the chained request exactly once: model=%q", out.Model)
	}
	// BYTE-EXACT pre-smuggler envelope at that hook boundary.
	if string(out.ProviderExtensions.Bytes()) != string(chat.ProviderExtensions.Bytes()) {
		t.Fatalf("the downstream envelope differs from the pre-plugin request:\n got %s\nwant %s",
			out.ProviderExtensions.Bytes(), chat.ProviderExtensions.Bytes())
	}
	if strings.Contains(string(out.ProviderExtensions.Bytes()), "smuggled-model") {
		t.Fatal("the smuggled member chained downstream")
	}
	if !out.CodeAssist {
		t.Fatal("typed variant lost")
	}
}

// pipelineWithApproval builds a pipeline with an explicit approval map.
func pipelineWithApproval(t testing.TB, dir string, order []string, approvals map[string]provider.PluginApproval, validator func(topo engine.TopologyFacts, current, replacement *pb.ChatRequest) error) *PluginPipeline {
	conv := map[string]Approval{}
	for name, a := range approvals {
		conv[name] = Approval{Digest: a.Digest, Permissions: a.Permissions, FailureMode: a.FailureMode}
	}
	t.Helper()
	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { rt.Close() })
	pp, err := NewPipeline(rt, PluginConfig{Dir: dir, Order: order, Approvals: conv, CandidateValidator: validator})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return pp
}

// pipelineWithValidator builds a pipeline with the candidate validator at
// CONSTRUCTION (the same seam production uses).
func pipelineWithValidator(t testing.TB, dir string, order []string, validator func(topo engine.TopologyFacts, current, replacement *pb.ChatRequest) error) *PluginPipeline {
	t.Helper()
	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { rt.Close() })
	pp, err := NewPipeline(rt, PluginConfig{Dir: dir, Order: order, AllowUnapproved: true, CandidateValidator: validator})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return pp
}
