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
