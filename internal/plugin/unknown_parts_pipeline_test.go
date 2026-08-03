package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format/gemini"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// geminiAdapter is the adapter the final-wire assertions marshal through.
var geminiAdapter = &gemini.Adapter{}

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
	want := map[string]string{}
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

// TestTypedHostFactsSurviveRealReplacement — a REAL replacement
// (test-mutator rewrites the user text) exercises the PB round-trip and
// the restoration path: ALL THREE typed host facts (Code Assist variant,
// OpenAI variant, Responses layout) are restored EXACTLY from the
// accepted request — a plugin can neither forge nor lose them — and the
// Code Assist envelope extras reach the final wire at their exact scopes.
func TestTypedHostFactsSurviveRealReplacement(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-mutator"})

	env, err := engine.ParseOptionalJSONObject([]byte(`{"project":"p-1","request":{"sessionId":"s-9"}}`))
	if err != nil {
		t.Fatal(err)
	}
	layout, err := engine.ParseOptionalJSONArray([]byte(`[{"type":"message","role":"user","content":"hi"},{"type":"reasoning","encrypted_content":"opaque"}]`))
	if err != nil {
		t.Fatal(err)
	}
	chat := &engine.ChatRequest{
		Model:                "gemini-3.5-flash",
		CodeAssist:           true,
		OpenAIVariant:        engine.OpenAIResponses,
		ResponsesInputLayout: layout,
		ProviderExtensions:   env,
		Messages:             []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 13, chat, nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	// The mutator rewrote the text (a REAL replacement happened).
	if out.Messages[0].Blocks[0].Text.Text == "hi" {
		t.Fatal("test-mutator did not replace (the pin is vacuous)")
	}
	// EXACT restoration of all three host-only facts.
	if !out.CodeAssist {
		t.Fatal("the typed Code Assist variant was lost through the replacement")
	}
	if out.OpenAIVariant != engine.OpenAIResponses {
		t.Fatalf("the OpenAI variant was lost: %v", out.OpenAIVariant)
	}
	if out.ResponsesInputLayout.IsAbsent() || string(out.ResponsesInputLayout.Bytes()) != string(layout.Bytes()) {
		t.Fatalf("the Responses layout was lost: %q", out.ResponsesInputLayout.Bytes())
	}
	// The envelope extras survive the replacement at their exact scopes.
	wire, err := geminiAdapter.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(wire, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["project"] != "p-1" {
		t.Fatalf("wrapper extra lost: %v", doc["project"])
	}
	req, ok := doc["request"].(map[string]any)
	if !ok {
		t.Fatalf("no request member: %s", wire)
	}
	if req["sessionId"] != "s-9" {
		t.Fatalf("inner extra lost: %v", req["sessionId"])
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
	pp := newTestPipeline(t, fixturesDir, []string{"test-envelope-smuggler"})
	pp.CandidateValidator = smuggleValidator

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
// replacement is dropped and the PRE-PLUGIN request chains to a downstream
// observer, which must see the untouched envelope.
func TestEnvelopeSmugglingPassRollsBackAndChains(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-envelope-smuggler/plugin.wasm")
	requireWASM(t, fixturesDir+"/test-inert-a/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-envelope-smuggler", "test-inert-a"})
	// The smuggler's manifest failure mode is block; override to pass via
	// an approval map so this pin exercises the pass semantics.
	digest, err := pluginBundleDigest(t, "test-envelope-smuggler")
	if err != nil {
		t.Fatal(err)
	}
	pp = pipelineWithApproval(t, fixturesDir, []string{"test-envelope-smuggler", "test-inert-a"}, map[string]provider.PluginApproval{
		"test-envelope-smuggler": {Digest: digest, Permissions: []string{"ir.params.write"}, FailureMode: "pass"},
	})
	pp.CandidateValidator = smuggleValidator

	chat := codeAssistRequest()
	out, err := pp.RunBeforeRequest(context.Background(), 21, chat, nil)
	if err != nil {
		t.Fatalf("pass mode must not error: %v", err)
	}
	if out == nil {
		t.Fatal("no chained output")
	}
	// The downstream chain saw the PRE-PLUGIN request: no smuggled model,
	// the envelope intact.
	ext := string(out.ProviderExtensions.Bytes())
	if strings.Contains(ext, "smuggled-model") {
		t.Fatalf("the smuggled member chained downstream: %s", ext)
	}
	if !out.CodeAssist {
		t.Fatal("typed variant lost")
	}
	if !strings.Contains(ext, `"sessionId":"s-9"`) {
		t.Fatalf("envelope altered through the pass: %s", ext)
	}
}

// pluginBundleDigest computes the digest of a fixture directory.
func pluginBundleDigest(t *testing.T, name string) (string, error) {
	t.Helper()
	return BundleDigestForDir(fixturesDir + "/" + name)
}

// pipelineWithApproval builds a pipeline with an explicit approval map.
func pipelineWithApproval(t testing.TB, dir string, order []string, approvals map[string]provider.PluginApproval) *PluginPipeline {
	conv := map[string]Approval{}
	for name, a := range approvals {
		conv[name] = Approval{Digest: a.Digest, Permissions: a.Permissions, FailureMode: a.FailureMode}
	}
	t.Helper()
	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { rt.Close() })
	pp, err := NewPipeline(rt, PluginConfig{Dir: dir, Order: order, Approvals: conv})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return pp
}
