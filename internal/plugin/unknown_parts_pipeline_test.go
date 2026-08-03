package plugin

import (
	"context"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

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
