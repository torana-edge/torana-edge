package format_test

import (
	"encoding/json"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/format"
	_ "github.com/torana-edge/torana-edge/internal/format/anthropic"
	_ "github.com/torana-edge/torana-edge/internal/format/gemini"
	_ "github.com/torana-edge/torana-edge/internal/format/openai"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
)

func TestPortableOutputFormatRoundTrip(t *testing.T) {
	cases := []struct{ name, provider, body string }{
		{"chat", "openai", `{"model":"m","messages":[{"role":"user","content":"question"}],"response_format":{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object","properties":{"answer":{"type":"string"}}}}}}`},
		{"responses", "openai", `{"model":"m","input":"question","text":{"verbosity":"low","format":{"type":"json_schema","name":"answer","strict":true,"schema":{"type":"object"}}}}`},
		{"anthropic", "anthropic", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"question"}],"output_config":{"effort":"low","format":{"type":"json_schema","schema":{"type":"object"}}}}`},
		{"gemini", "gemini", `{"contents":[{"role":"user","parts":[{"text":"question"}]}],"generationConfig":{"temperature":0.2,"candidateCount":1,"responseMimeType":"application/json","responseJsonSchema":{"type":"object"}}}`},
		{"codeassist", "gemini", `{"model":"m","request":{"contents":[{"role":"user","parts":[{"text":"question"}]}],"generationConfig":{"responseMimeType":"application/json","responseJsonSchema":{"type":"object"}}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := format.Lookup(tc.provider).Request
			request, err := adapter.Unmarshal([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if request.OutputFormat == nil || request.OutputFormat.Mode != 2 {
				t.Fatal("constraint stayed opaque")
			}
			canonical, err := pbconv.ToPBChatRequestChecked(request)
			if err != nil {
				t.Fatal(err)
			}
			projected, err := pbconv.FromPBChatRequest(canonical)
			if err != nil {
				t.Fatal(err)
			}
			if projected.OutputFormat == nil || projected.OutputFormat.Schema.String() != request.OutputFormat.Schema.String() {
				t.Fatal("protobuf conversion lost schema")
			}
			raw, err := adapter.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			again, err := adapter.Unmarshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			againPB, err := pbconv.ToPBChatRequestChecked(again)
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(againPB.OutputFormat, canonical.OutputFormat) {
				t.Fatalf("constraint drift: %s", raw)
			}
			var original, result map[string]any
			json.Unmarshal([]byte(tc.body), &original)
			json.Unmarshal(raw, &result)
			if tc.name == "responses" && result["text"].(map[string]any)["verbosity"] != "low" {
				t.Fatal("lost opaque text sibling")
			}
			if tc.name == "anthropic" && result["output_config"].(map[string]any)["effort"] != "low" {
				t.Fatal("lost output config sibling")
			}
		})
	}
}

func TestPortableOutputFormatRefusesUnsupportedMode(t *testing.T) {
	for _, provider := range []string{"anthropic", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			strict := true
			request := &engine.ChatRequest{Model: "m", OutputFormat: &engine.OutputFormat{Mode: 2, Name: "answer", Strict: &strict}}
			request.OutputFormat.Schema, _ = engine.ParseOptionalJSONObject([]byte(`{"type":"object"}`))
			if _, err := format.Lookup(provider).Request.Marshal(request); err == nil {
				t.Fatal("silently dropped explicit strict option")
			}
		})
	}
}

func TestOutputFormatIsPartOfObservablePrefix(t *testing.T) {
	req := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{{Role: "user", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "question"}}}, {Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: []byte(`{"type":"ephemeral"}`)}}}}}}}
	before := engine.CachePrefixKey(req)
	req.OutputFormat = &pb.OutputFormat{Mode: pb.OutputFormat_JSON_SCHEMA, Name: "answer", SchemaJson: []byte(`{"type":"object"}`)}
	after := engine.CachePrefixKey(req)
	if before == "" || after == "" || before == after {
		t.Fatalf("output constraint missing from cache identity: %s -> %s", before, after)
	}
}

func TestOutputFormatPreservesUnmodeledProviderFields(t *testing.T) {
	cases := []struct{ provider, body string }{
		{"openai", `{"model":"m","messages":[],"response_format":{"type":"json_schema","json_schema":{"name":"answer","description":"keep me","schema":{"type":"object"}}}}`},
		{"openai", `{"model":"m","input":"question","text":{"format":{"type":"json_schema","name":"answer","description":"keep me","schema":{"type":"object"}}}}`},
		{"anthropic", `{"model":"m","max_tokens":10,"messages":[],"output_config":{"format":{"type":"json_schema","schema":{"type":"object"},"future_option":"keep me"}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.provider+tc.body, func(t *testing.T) {
			adapter := format.Lookup(tc.provider).Request
			request, err := adapter.Unmarshal([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if request.OutputFormat != nil {
				t.Fatal("partially modeled constraint was removed from provider extras")
			}
			raw, err := adapter.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			var original, actual map[string]any
			if err = json.Unmarshal([]byte(tc.body), &original); err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(raw, &actual); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"response_format", "text", "output_config"} {
				if want, ok := original[key]; ok {
					wb, _ := json.Marshal(want)
					gb, _ := json.Marshal(actual[key])
					if string(wb) != string(gb) {
						t.Fatalf("lost provider options: %s want %s", gb, wb)
					}
				}
			}
			request.OutputFormat = &engine.OutputFormat{Mode: 1}
			if _, err = adapter.Marshal(request); err == nil {
				t.Fatal("typed output options silently overwrote opaque constraint")
			}
		})
	}
}
