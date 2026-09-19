package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/format"
	_ "github.com/torana-edge/torana-edge/internal/format/anthropic"
	_ "github.com/torana-edge/torana-edge/internal/format/gemini"
	_ "github.com/torana-edge/torana-edge/internal/format/openai"
)

func projectAdversarialRequest(t *testing.T, from, to Protocol, body string) ([]byte, error) {
	t.Helper()
	chat, err := ParseRequest(from, []byte(body), requestPath(from))
	if err != nil {
		return nil, err
	}
	projected, err := ProjectRequest(chat, from, to, RequestOptions{MaxTokens: 64, Project: "destination-project"})
	if err != nil {
		return nil, err
	}
	return format.Lookup(to.Format()).Request.Marshal(projected)
}

func requireAdversarialUnsupported(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("semantic loss was accepted")
	}
	var unsupportedErr *UnsupportedError
	if !errors.As(err, &unsupportedErr) {
		t.Fatalf("error = %T %v, want UnsupportedError", err, err)
	}
	if strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("error exposed a request value: %v", err)
	}
}

// These fields live in wire locations that the provider adapters project into
// narrower engine types. The bridge must reject them before that parse, rather
// than approve the already-narrowed request after their semantics disappeared.
func TestAdversarialRequestRejectsFieldsBeforeAdapterCanDropThem(t *testing.T) {
	rows := []struct {
		name     string
		from, to Protocol
		body     string
	}{
		{
			name: "named chat participant",
			from: OpenAIChat, to: Anthropic,
			body: `{"model":"m","messages":[{"role":"user","name":"sensitive-participant","content":"hello"}],"max_tokens":8}`,
		},
		{
			name: "destination cannot carry developer role",
			from: OpenAIChat, to: Anthropic,
			body: `{"model":"m","messages":[{"role":"developer","content":"sensitive instruction"},{"role":"user","content":"hello"}],"max_tokens":8}`,
		},
		{
			name: "anthropic known tool block extension",
			from: Anthropic, to: OpenAIChat,
			body: `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{},"caller":{"kind":"sensitive"}}]}]}`,
		},
		{
			name: "gemini function call extension",
			from: Gemini, to: OpenAIChat,
			body: `{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call_1","name":"lookup","args":{},"behavior":"sensitive"}}]}],"generationConfig":{"maxOutputTokens":8}}`,
		},
		{
			name: "gemini system role",
			from: Gemini, to: OpenAIChat,
			body: `{"systemInstruction":{"role":"model","parts":[{"text":"sensitive instruction"}]},"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":8}}`,
		},
		{
			name: "nonterminal Responses function history",
			from: OpenAIResponses, to: Anthropic,
			body: `{"model":"m","input":[{"type":"function_call","id":"item_1","status":"in_progress","call_id":"call_1","name":"lookup","arguments":"{}"}],"max_output_tokens":8}`,
		},
		{
			name: "nonterminal Responses message history",
			from: OpenAIResponses, to: Anthropic,
			body: `{"model":"m","input":[{"type":"message","id":"item_1","status":"in_progress","role":"assistant","content":[{"type":"output_text","text":"sensitive partial"}]}],"max_output_tokens":8}`,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := projectAdversarialRequest(t, row.from, row.to, row.body)
			requireAdversarialUnsupported(t, err)
		})
	}
}

func TestAdversarialRequestPreservesTwoSequentialToolRounds(t *testing.T) {
	const body = `{
  "model":"source","max_tokens":32,
  "messages":[
    {"role":"user","content":"first"},
    {"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{\"round\":1}"}}]},
    {"role":"tool","tool_call_id":"call_a","name":"lookup","content":"one"},
    {"role":"assistant","tool_calls":[{"id":"call_b","type":"function","function":{"name":"lookup","arguments":"{\"round\":2}"}}]},
    {"role":"tool","tool_call_id":"call_b","name":"lookup","content":"two"}
  ],
  "tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]
}`

	for _, to := range []Protocol{OpenAIResponses, Anthropic, Gemini, GeminiCodeAssist} {
		t.Run(string(to), func(t *testing.T) {
			wire, err := projectAdversarialRequest(t, OpenAIChat, to, body)
			if err != nil {
				t.Fatal(err)
			}
			reparsed, err := ParseRequest(to, wire, requestPath(to))
			if err != nil {
				t.Fatalf("translated request cannot be reparsed: %v\n%s", err, wire)
			}
			var sequence []string
			for _, msg := range reparsed.Messages {
				for _, block := range msg.Blocks {
					switch {
					case block.ToolUse != nil:
						sequence = append(sequence, "call:"+block.ToolUse.ID)
					case block.ToolResult != nil:
						sequence = append(sequence, "result:"+block.ToolResult.ToolCallID)
					}
				}
			}
			want := []string{"call:call_a", "result:call_a", "call:call_b", "result:call_b"}
			if fmt.Sprint(sequence) != fmt.Sprint(want) {
				t.Fatalf("tool round order = %v, want %v\n%s", sequence, want, wire)
			}
		})
	}
}

func TestAdversarialRequestCarriesRecoverableToolResultErrors(t *testing.T) {
	const body = `{
  "model":"claude","max_tokens":16,
  "messages":[
    {"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{}}]},
    {"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","is_error":true,"content":"sensitive failure"}]}
  ]
}`
	for _, to := range []Protocol{OpenAIChat, OpenAIResponses, Gemini, GeminiCodeAssist} {
		t.Run(string(to), func(t *testing.T) {
			wire, err := projectAdversarialRequest(t, Anthropic, to, body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(wire, []byte("sensitive failure")) {
				t.Fatalf("recoverable diagnostic was dropped: %s", wire)
			}
			if (to == Gemini || to == GeminiCodeAssist) && !bytes.Contains(wire, []byte(`"error"`)) {
				t.Fatalf("Gemini error object missing: %s", wire)
			}
		})
	}
}

func TestAdversarialRequestRejectsSignaturesAndCacheAtEveryCarrier(t *testing.T) {
	rows := []struct {
		name string
		from Protocol
		body string
	}{
		{
			name: "anthropic system cache", from: Anthropic,
			body: `{"model":"m","max_tokens":8,"system":[{"type":"text","text":"sensitive","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"x"}]}`,
		},
		{
			name: "anthropic tool cache", from: Anthropic,
			body: `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"lookup","input_schema":{},"cache_control":{"type":"ephemeral"}}]}`,
		},
		{
			name: "anthropic nested result cache", from: Anthropic,
			body: `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"sensitive","cache_control":{"type":"ephemeral"}}]}]}]}`,
		},
		{
			name: "anthropic thinking signature", from: Anthropic,
			body: `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"sensitive","signature":"opaque"}]}]}`,
		},
		{
			name: "gemini text signature", from: Gemini,
			body: `{"contents":[{"role":"model","parts":[{"text":"sensitive","thoughtSignature":"opaque"}]}],"generationConfig":{"maxOutputTokens":8}}`,
		},
		{
			name: "gemini result signature", from: Gemini,
			body: `{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call_1","name":"lookup","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"id":"call_1","name":"lookup","response":{"output":"sensitive"}},"thoughtSignature":"opaque"}]}],"generationConfig":{"maxOutputTokens":8}}`,
		},
		{
			name: "codeassist trailing signature", from: GeminiCodeAssist,
			body: `{"model":"m","project":"p","request":{"contents":[{"role":"model","parts":[{"text":"sensitive"},{"text":"","thoughtSignature":"opaque"}]}],"generationConfig":{"maxOutputTokens":8}}}`,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := projectAdversarialRequest(t, row.from, OpenAIChat, row.body)
			requireAdversarialUnsupported(t, err)
		})
	}
}

func TestAdversarialRequestProjectsPortableInlineImages(t *testing.T) {
	const image = "aGVsbG8="
	const body = `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + image + `"}}]}]}`
	for _, to := range []Protocol{OpenAIResponses, Anthropic, Gemini, GeminiCodeAssist} {
		t.Run(string(to), func(t *testing.T) {
			wire, err := projectAdversarialRequest(t, OpenAIChat, to, body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(wire, []byte(image)) {
				t.Fatalf("image bytes disappeared: %s", wire)
			}
			reparsed, err := ParseRequest(to, wire, requestPath(to))
			if err != nil {
				t.Fatalf("translated image cannot be reparsed: %v\n%s", err, wire)
			}
			unknown := 0
			for _, msg := range reparsed.Messages {
				for _, block := range msg.Blocks {
					if block.Unknown != nil {
						unknown++
					}
				}
			}
			if unknown != 1 {
				t.Fatalf("image block count = %d, want 1", unknown)
			}
		})
	}
}

func TestAdversarialRequestRejectsUnportableImages(t *testing.T) {
	rows := []struct {
		name     string
		from, to Protocol
		body     string
	}{
		{
			name: "remote image into Gemini", from: OpenAIChat, to: Gemini,
			body: `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/sensitive.png"}}]}]}`,
		},
		{
			name: "OpenAI high detail", from: OpenAIChat, to: Anthropic,
			body: `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8=","detail":"high"}}]}]}`,
		},
		{
			name: "Anthropic document", from: Anthropic, to: OpenAIChat,
			body: `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"c2Vuc2l0aXZl"}}]}]}`,
		},
		{
			name: "Gemini file reference", from: Gemini, to: OpenAIChat,
			body: `{"contents":[{"role":"user","parts":[{"fileData":{"mimeType":"image/png","fileUri":"gs://sensitive/object"}}]}],"generationConfig":{"maxOutputTokens":8}}`,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := projectAdversarialRequest(t, row.from, row.to, row.body)
			requireAdversarialUnsupported(t, err)
		})
	}
}

func TestAdversarialRequestProjectsStructuredOutputConstraints(t *testing.T) {
	const schemaMarker = `"required":["n"]`
	rows := []struct {
		name     string
		from, to Protocol
		body     string
		markers  []string
	}{
		{
			name: "Chat schema to Responses", from: OpenAIChat, to: OpenAIResponses,
			body:    `{"model":"m","messages":[{"role":"user","content":"x"}],"max_tokens":8,"response_format":{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}}}}`,
			markers: []string{`"text"`, `"format"`, `"name":"answer"`, `"strict":true`, schemaMarker},
		},
		{
			name: "Responses schema to Gemini", from: OpenAIResponses, to: Gemini,
			body:    `{"model":"m","input":"x","max_output_tokens":8,"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}}}}`,
			markers: []string{`"responseMimeType":"application/json"`, `"responseJsonSchema"`, schemaMarker},
		},
		{
			name: "Anthropic schema to Chat", from: Anthropic, to: OpenAIChat,
			body:    `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"x"}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}}}}`,
			markers: []string{`"response_format"`, `"json_schema"`, schemaMarker},
		},
		{
			name: "Gemini schema to Chat", from: Gemini, to: OpenAIChat,
			body:    `{"contents":[{"role":"user","parts":[{"text":"x"}]}],"generationConfig":{"maxOutputTokens":8,"responseMimeType":"application/json","responseJsonSchema":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}}}`,
			markers: []string{`"response_format"`, `"json_schema"`, schemaMarker},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			wire, err := projectAdversarialRequest(t, row.from, row.to, row.body)
			if err != nil {
				t.Fatal(err)
			}
			for _, marker := range row.markers {
				if !bytes.Contains(wire, []byte(marker)) {
					t.Fatalf("structured-output marker %s missing: %s", marker, wire)
				}
			}
		})
	}
}

func TestAdversarialRequestRejectsWeakenedOutputConstraints(t *testing.T) {
	rows := []struct {
		name     string
		from, to Protocol
		body     string
	}{
		{
			name: "strict schema into Gemini", from: OpenAIChat, to: Gemini,
			body: `{"model":"m","messages":[{"role":"user","content":"x"}],"max_tokens":8,"response_format":{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object"}}}}`,
		},
		{
			name: "JSON object mode into Anthropic", from: OpenAIChat, to: Anthropic,
			body: `{"model":"m","messages":[{"role":"user","content":"x"}],"max_tokens":8,"response_format":{"type":"json_object"}}`,
		},
		{
			name: "legacy Gemini schema dialect", from: Gemini, to: OpenAIChat,
			body: `{"contents":[{"role":"user","parts":[{"text":"x"}]}],"generationConfig":{"maxOutputTokens":8,"responseMimeType":"application/json","responseSchema":{"type":"OBJECT","required":["sensitive"]}}}`,
		},
		{
			name: "Responses format extension", from: OpenAIResponses, to: Gemini,
			body: `{"model":"m","input":"x","max_output_tokens":8,"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"},"verbosity":"sensitive"}}}`,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := projectAdversarialRequest(t, row.from, row.to, row.body)
			requireAdversarialUnsupported(t, err)
		})
	}
}

func TestAdversarialRequestErrorsDoNotEchoUnknownMemberNames(t *testing.T) {
	const body = `{"model":"m","messages":[{"role":"user","content":"x","sensitive-member-name":true}],"max_tokens":8}`
	_, err := projectAdversarialRequest(t, OpenAIChat, Anthropic, body)
	requireAdversarialUnsupported(t, err)
}

func TestAdversarialRequestStructuredOutputRemainsRawIntegerSafe(t *testing.T) {
	const huge = "9007199254740993123456789"
	body := `{"model":"m","input":"x","max_output_tokens":8,"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"n":{"type":"integer","maximum":` + huge + `}}}}}}`
	wire, err := projectAdversarialRequest(t, OpenAIResponses, OpenAIChat, body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(wire, []byte(huge)) {
		t.Fatalf("schema integer lexeme changed: %s", wire)
	}
	var valid any
	if err := json.Unmarshal(wire, &valid); err != nil {
		t.Fatalf("translated request is malformed: %v", err)
	}
}
