package format_test

// Representation pins for the required-fields migration (PR A commit 3):
// tool-call arguments and tool schemas travel as authoritative raw lexemes
// through every format adapter, PB conversion, and the cache prefix. These
// tests pin the per-format round trips (1e999, large integers, 1.0,
// escaped Unicode, non-alphabetical key order), the nil-to-{} normalization
// (empty provider arguments/schemas become the canonical {}), and the
// error path (malformed PB arguments cannot silently become nil).

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/format/anthropic"
	"github.com/torana-edge/torana-edge/internal/format/bedrock"
	"github.com/torana-edge/torana-edge/internal/format/gemini"
	"github.com/torana-edge/torana-edge/internal/format/openai"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// Ordered-body test helpers (engine.Message).

type tcView struct {
	ID, Name  string
	Args      engine.RequiredJSONObject
	Signature string
}

func toolCalls(m engine.Message) []tcView {
	var out []tcView
	for _, b := range m.Blocks {
		if b.ToolUse != nil {
			out = append(out, tcView{ID: b.ToolUse.ID, Name: b.ToolUse.Name, Args: b.ToolUse.Arguments, Signature: b.ToolUse.Signature})
		}
	}
	return out
}

// lexemeCases are the provider-valid lexemes that must survive verbatim.
var lexemeCases = []struct {
	name string
	json string // object body used as arguments / parameters
	want []string
}{
	{"big number", `{"z":1e999,"a":18446744073709551615,"o":1.0}`, []string{"1e999", "18446744073709551615", "1.0"}},
	{"escaped unicode", `{"s":"\ud83d\ude00","e":"\u0061"}`, []string{`\ud83d\ude00`, `\u0061`}},
	{"non-alphabetical order", `{"z":1,"a":2,"m":3}`, []string{`"z":1,"a":2,"m":3`}},
}

// TestRawJSONArgumentsRoundTripAnthropic: tool_use input lexemes survive
// unmarshal -> wrapper -> marshal.
func TestRawJSONArgumentsRoundTripAnthropic(t *testing.T) {
	for _, tc := range lexemeCases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read_file","input":` + tc.json + `}]}]}`
			chat, err := (&anthropic.Adapter{}).Unmarshal([]byte(body))
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := toolCalls(chat.Messages[0])[0].Args.String()
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Fatalf("arguments lexeme %q lost: %s", w, got)
				}
			}
			out, err := (&anthropic.Adapter{}).Marshal(chat)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(string(out), w) {
					t.Fatalf("wire lexeme %q lost: %s", w, out)
				}
			}
		})
	}
}

// TestRawJSONArgumentsRoundTripOpenAI: the arguments JSON TEXT (decoded once
// at the parse boundary) survives as exact lexemes inside the wire string.
func TestRawJSONArgumentsRoundTripOpenAI(t *testing.T) {
	for _, tc := range lexemeCases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"t1","type":"function","function":{"name":"read_file","arguments":` +
				jsonMarshalString(tc.json) + `}}]}]}`
			chat, err := (&openai.Adapter{}).Unmarshal([]byte(body))
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := toolCalls(chat.Messages[0])[0].Args.String()
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Fatalf("arguments lexeme %q lost: %s", w, got)
				}
			}
			out, err := (&openai.Adapter{}).Marshal(chat)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			// OpenAI embeds the arguments JSON TEXT inside a string, so the
			// wire carries the escaped rendering of the exact lexemes.
			escaped := jsonMarshalString(got)
			inner := escaped[1 : len(escaped)-1]
			if !strings.Contains(string(out), inner) {
				t.Fatalf("wire arguments text lost: %s", out)
			}
		})
	}
}

// TestRawJSONArgumentsRoundTripGemini: functionCall args lexemes survive.
func TestRawJSONArgumentsRoundTripGemini(t *testing.T) {
	for _, tc := range lexemeCases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"read_file","args":` + tc.json + `}}]}]}`
			chat, err := (&gemini.Adapter{}).Unmarshal([]byte(body))
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := toolCalls(chat.Messages[0])[0].Args.String()
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Fatalf("arguments lexeme %q lost: %s", w, got)
				}
			}
			out, err := (&gemini.Adapter{}).Marshal(chat)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(string(out), w) {
					t.Fatalf("wire lexeme %q lost: %s", w, out)
				}
			}
		})
	}
}

// TestRawJSONArgumentsRoundTripBedrock: toolUse input lexemes survive.
func TestRawJSONArgumentsRoundTripBedrock(t *testing.T) {
	for _, tc := range lexemeCases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"modelId":"m","messages":[{"role":"assistant","content":[{"toolUse":{"toolUseId":"t1","name":"read_file","input":` + tc.json + `}}]}]}`
			chat, err := (&bedrock.Adapter{}).Unmarshal([]byte(body))
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := toolCalls(chat.Messages[0])[0].Args.String()
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Fatalf("arguments lexeme %q lost: %s", w, got)
				}
			}
			out, err := (&bedrock.Adapter{}).Marshal(chat)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(string(out), w) {
					t.Fatalf("wire lexeme %q lost: %s", w, out)
				}
			}
		})
	}
}

// TestRawJSONParametersRoundTrip: tool schemas with non-alphabetical key
// order survive unmarshal -> wrapper -> marshal for every format that
// carries them.
func TestRawJSONParametersRoundTrip(t *testing.T) {
	schema := `{"z":1,"properties":{"a":{"type":"string"},"m":{"type":"number"}},"required":["a"],"type":"object"}`
	adapters := []struct {
		name      string
		unmarshal func([]byte) (*engine.ChatRequest, error)
		marshal   func(*engine.ChatRequest) ([]byte, error)
		body      string
	}{
		{"anthropic", func(b []byte) (*engine.ChatRequest, error) { return (&anthropic.Adapter{}).Unmarshal(b) },
			func(c *engine.ChatRequest) ([]byte, error) { return (&anthropic.Adapter{}).Marshal(c) },
			`{"model":"m","tools":[{"name":"read_file","input_schema":` + schema + `}],"messages":[]}`},
		{"openai", func(b []byte) (*engine.ChatRequest, error) { return (&openai.Adapter{}).Unmarshal(b) },
			func(c *engine.ChatRequest) ([]byte, error) { return (&openai.Adapter{}).Marshal(c) },
			`{"model":"m","tools":[{"type":"function","function":{"name":"read_file","parameters":` + schema + `}}],"messages":[]}`},
		{"gemini", func(b []byte) (*engine.ChatRequest, error) { return (&gemini.Adapter{}).Unmarshal(b) },
			func(c *engine.ChatRequest) ([]byte, error) { return (&gemini.Adapter{}).Marshal(c) },
			`{"contents":[],"tools":[{"functionDeclarations":[{"name":"read_file","parameters":` + schema + `}]}]}`},
		{"bedrock", func(b []byte) (*engine.ChatRequest, error) { return (&bedrock.Adapter{}).Unmarshal(b) },
			func(c *engine.ChatRequest) ([]byte, error) { return (&bedrock.Adapter{}).Marshal(c) },
			`{"modelId":"m","toolConfig":{"tools":[{"toolSpec":{"name":"read_file","inputSchema":{"json":` + schema + `}}}]},"messages":[]}`},
	}
	for _, a := range adapters {
		t.Run(a.name, func(t *testing.T) {
			chat, err := a.unmarshal([]byte(a.body))
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := chat.Tools[0].Parameters.String()
			for _, w := range []string{`"z":1`, `"a":{"type":"string"}`, `"m":{"type":"number"}`} {
				if !strings.Contains(got, w) {
					t.Fatalf("schema member %q lost: %s", w, got)
				}
			}
			out, err := a.marshal(chat)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			for _, w := range []string{`"z":1`, `"m":{"type":"number"}`} {
				if !strings.Contains(string(out), w) {
					t.Fatalf("wire schema member %q lost: %s", w, out)
				}
			}
		})
	}
}

// TestRawJSONNilNormalization: provider inputs with NO arguments/schema
// normalize to the canonical {} — never to absence — at the wrapper and on
// the wire and in the PB.

func mustCheckedPB(t *testing.T, chat *engine.ChatRequest) *pb.ChatRequest {
	t.Helper()
	pbReq, err := pbconv.ToPBChatRequestChecked(chat)
	if err != nil {
		t.Fatal(err)
	}
	return pbReq
}

func TestRawJSONNilNormalization(t *testing.T) {
	// OpenAI: empty arguments string.
	body := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"t1","type":"function","function":{"name":"read_file","arguments":""}}]}]}`
	chat, err := (&openai.Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := toolCalls(chat.Messages[0])[0].Args.String(); got != `{}` {
		t.Fatalf("empty arguments string = %q, want {}", got)
	}
	// Anthropic: tool_use without input.
	body = `{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read_file"}]}]}`
	chat, err = (&anthropic.Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := toolCalls(chat.Messages[0])[0].Args.String(); got != `{}` {
		t.Fatalf("missing input = %q, want {}", got)
	}
	// Gemini: tool without parameters -> {} schema (unconstrained).
	body = `{"contents":[],"tools":[{"functionDeclarations":[{"name":"read_file"}]}]}`
	chat, err = (&gemini.Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := chat.Tools[0].Parameters.String(); got != `{}` {
		t.Fatalf("missing parameters = %q, want {}", got)
	}
	// PB conversion emits {} for the zero wrapper.
	pbReq := mustCheckedPB(t, chat)
	if len(pbReq.Tools) != 1 || string(pbReq.Tools[0].ParametersJson) != `{}` {
		t.Fatalf("PB parameters = %q, want {}", pbReq.Tools[0].ParametersJson)
	}
}

// TestRawJSONFromPBError: malformed arguments_json in a PB cannot silently
// become a nil/partial engine value — FromPBChatRequest errors.
func TestRawJSONFromPBError(t *testing.T) {
	good := mustCheckedPB(t, &engine.ChatRequest{Messages: []engine.Message{
		{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{
			ID: "t1", Name: "read", Arguments: mustPinned(`{"path":"x"}`),
		}}}},
	}})
	good.Messages[0].Blocks[0].GetToolUse().ArgumentsJson = []byte(`[1,2]`)
	if _, err := pbconv.FromPBChatRequest(good); err == nil {
		t.Fatal("malformed arguments_json accepted by FromPBChatRequest")
	}
	bad := &pb.ChatRequest{Tools: []*pb.ToolDef{{Name: "t", ParametersJson: []byte(`"nope"`)}}}
	if _, err := pbconv.FromPBChatRequest(bad); err == nil {
		t.Fatal("wrong-shape parameters_json accepted by FromPBChatRequest")
	}
}

// TestRawJSONCacheKeyPins: cache identity follows the raw lexemes — two
// requests identical except the arguments lexeme differ in key; the same
// bytes in different fields differ; the canonical {} is deterministic.
func TestRawJSONCacheKeyPins(t *testing.T) {
	base := func(args, params string) *engine.ChatRequest {
		return &engine.ChatRequest{
			Model: "m",
			Messages: []engine.Message{
				{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}},
				{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{
					ID: "t1", Name: "read", Arguments: mustPinned(args),
				}}}},
			},
			Tools: []engine.ToolDef{{Name: "read", Parameters: mustPinned(params)}},
		}
	}
	// Lexeme difference changes the key.
	if k1, k2 := mustCacheKey(t, base(`{"path":"a"}`, `{"type":"object"}`)),
		mustCacheKey(t, base(`{"path":"a","x":1e999}`, `{"type":"object"}`)); k1 == k2 {
		t.Fatal("lexeme difference did not change the cache key")
	}
	// Key order difference changes the key.
	if k1, k2 := mustCacheKey(t, base(`{"x":1e999,"path":"a"}`, `{"type":"object"}`)),
		mustCacheKey(t, base(`{"path":"a","x":1e999}`, `{"type":"object"}`)); k1 == k2 {
		t.Fatal("key order difference did not change the cache key")
	}
	// Field identity/position is part of the framed transcript: the SAME two
	// byte payloads swapped between arguments and parameters must NOT share
	// a key (they would collide if writeHashFieldBytes alone carried the
	// bytes without the surrounding positional framing).
	payloadA := `{"path":"a"}`
	payloadB := `{"type":"object"}`
	ab := mustCacheKey(t, base(payloadA, payloadB))
	ba := mustCacheKey(t, base(payloadB, payloadA))
	if ab == ba {
		t.Fatal("identical bytes in different fields produced the same cache key")
	}
	// Positive controls: identical requests and canonical-zero wrappers are
	// deterministic and equal.
	if k1, k2 := mustCacheKey(t, base(`{}`, `{}`)), mustCacheKey(t, base(`{}`, `{}`)); k1 != k2 {
		t.Fatal("canonical {} cache key is not deterministic")
	}
	if k1, k2 := mustCacheKey(t, base(`{"path":"a"}`, `{"type":"object"}`)),
		mustCacheKey(t, base(`{"path":"a"}`, `{"type":"object"}`)); k1 != k2 {
		t.Fatal("identical requests produced different cache keys")
	}
}

func mustPinned(raw string) engine.RequiredJSONObject {
	r, err := engine.ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		panic(err)
	}
	return r
}

// jsonMarshalString returns the JSON string encoding of raw (the OpenAI
// wire shape embeds JSON text inside a string).
func jsonMarshalString(raw string) string {
	b, _ := json.Marshal(raw)
	return string(b)
}

// TestRawJSONFromPBNilGraph: the converter is TOTAL over arbitrary
// protobuf object graphs — nil nested messages, tool defs, and tool calls
// are indexed refusals, never panics, never silent empties.
func TestRawJSONFromPBNilGraph(t *testing.T) {
	cases := []struct {
		name string
		req  *pb.ChatRequest
	}{
		{"nil message element", &pb.ChatRequest{Messages: []*pb.Message{nil}}},
		{"nil tool def element", &pb.ChatRequest{Tools: []*pb.ToolDef{nil}}},
		{"nil block element", &pb.ChatRequest{Messages: []*pb.Message{
			{Role: "assistant", Blocks: []*pb.RequestBlock{nil}},
		}}},
		{"nil message inside valid list", &pb.ChatRequest{Messages: []*pb.Message{
			{Role: "user", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "hi"}}}}}, nil,
		}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := pbconv.FromPBChatRequest(c.req); err == nil {
				t.Fatal("nil graph accepted")
			}
		})
	}
}

// TestRawJSONMatrixExact: the full lexeme corpus runs for BOTH required
// fields on EVERY applicable format arm (OpenAI Chat AND Responses), with
// exact boundary equality: wrapper.Bytes() equals the expected input
// representation, and the marshal output extracts the exact raw object
// (or the exact JSON-text string for OpenAI) which must byte-equal it.
func TestRawJSONMatrixExact(t *testing.T) {
	corpus := []struct {
		name string
		obj  string // the object representation expected at the engine boundary
	}{
		{"big number", `{"z":1e999,"a":18446744073709551615,"o":1.0}`},
		{"escaped unicode", `{"s":"\ud83d\ude00","e":"\u0061"}`},
		{"non-alphabetical order", `{"z":1,"a":2,"m":3}`},
		{"whitespace members", `{ "z" : 1 , "a" : 2 }`},
	}
	type arm struct {
		name      string
		body      func(string, string) string // format body with args+params inserted
		unmarshal func([]byte) (*engine.ChatRequest, error)
		marshal   func(*engine.ChatRequest) ([]byte, error)
		// argsExactText marks arms where the arguments travel as a JSON
		// TEXT STRING (OpenAI): string content survives byte-exact,
		// including intra-value whitespace. Every other raw field rides in
		// a struct json.RawMessage, and Go's encoding/json COMPACTS
		// Marshaler output in struct fields (documented behaviour), so the
		// wire carries the lexemes, key order, and content with
		// insignificant whitespace deterministically removed.
		argsExactText bool
		extractArgs   func(t *testing.T, out []byte) []byte
		extractParams func(t *testing.T, out []byte) []byte
	}
	arms := []arm{
		{
			name: "anthropic",
			body: func(args, params string) string {
				return `{"model":"m","tools":[{"name":"read_file","input_schema":` + params + `}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read_file","input":` + args + `}]}]}`
			},
			unmarshal: func(b []byte) (*engine.ChatRequest, error) { return (&anthropic.Adapter{}).Unmarshal(b) },
			marshal:   func(c *engine.ChatRequest) ([]byte, error) { return (&anthropic.Adapter{}).Marshal(c) },
			extractArgs: func(t *testing.T, out []byte) []byte {
				var m struct {
					Messages []struct {
						Content []struct {
							Type  string          `json:"type"`
							Input json.RawMessage `json:"input"`
						} `json:"content"`
					} `json:"messages"`
				}
				if err := json.Unmarshal(out, &m); err != nil {
					t.Fatalf("extract: %v", err)
				}
				return m.Messages[0].Content[0].Input
			},
			extractParams: func(t *testing.T, out []byte) []byte {
				var m struct {
					Tools []struct {
						InputSchema json.RawMessage `json:"input_schema"`
					} `json:"tools"`
				}
				if err := json.Unmarshal(out, &m); err != nil {
					t.Fatalf("extract: %v", err)
				}
				return m.Tools[0].InputSchema
			},
		},
		{
			name:          "openai-chat",
			argsExactText: true,
			body: func(args, params string) string {
				return `{"model":"m","tools":[{"type":"function","function":{"name":"read_file","parameters":` + params + `}}],"messages":[{"role":"assistant","tool_calls":[{"id":"t1","type":"function","function":{"name":"read_file","arguments":` + jsonMarshalString(args) + `}}]}]}`
			},
			unmarshal: func(b []byte) (*engine.ChatRequest, error) { return (&openai.Adapter{}).Unmarshal(b) },
			marshal:   func(c *engine.ChatRequest) ([]byte, error) { return (&openai.Adapter{}).Marshal(c) },
			extractArgs: func(t *testing.T, out []byte) []byte {
				var m struct {
					Messages []struct {
						ToolCalls []struct {
							Function struct {
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"messages"`
				}
				if err := json.Unmarshal(out, &m); err != nil {
					t.Fatalf("extract: %v", err)
				}
				return []byte(m.Messages[0].ToolCalls[0].Function.Arguments)
			},
			extractParams: func(t *testing.T, out []byte) []byte {
				var m struct {
					Tools []struct {
						Function struct {
							Parameters json.RawMessage `json:"parameters"`
						} `json:"function"`
					} `json:"tools"`
				}
				if err := json.Unmarshal(out, &m); err != nil {
					t.Fatalf("extract: %v", err)
				}
				return m.Tools[0].Function.Parameters
			},
		},
		{
			name:          "openai-responses",
			argsExactText: true,
			body: func(args, params string) string {
				return `{"model":"m","input":[{"type":"function_call","call_id":"t1","name":"read_file","arguments":` + jsonMarshalString(args) + `}],"tools":[{"type":"function","name":"read_file","parameters":` + params + `}]}`
			},
			unmarshal: func(b []byte) (*engine.ChatRequest, error) { return (&openai.Adapter{}).Unmarshal(b) },
			marshal:   func(c *engine.ChatRequest) ([]byte, error) { return (&openai.Adapter{}).Marshal(c) },
			extractArgs: func(t *testing.T, out []byte) []byte {
				var m struct {
					Input []struct {
						Type      string `json:"type"`
						Arguments string `json:"arguments"`
					} `json:"input"`
				}
				if err := json.Unmarshal(out, &m); err != nil {
					t.Fatalf("extract: %v", err)
				}
				return []byte(m.Input[0].Arguments)
			},
			extractParams: func(t *testing.T, out []byte) []byte {
				var m struct {
					Tools []struct {
						Parameters json.RawMessage `json:"parameters"`
					} `json:"tools"`
				}
				if err := json.Unmarshal(out, &m); err != nil {
					t.Fatalf("extract: %v", err)
				}
				return m.Tools[0].Parameters
			},
		},
		{
			name: "gemini",
			body: func(args, params string) string {
				return `{"tools":[{"functionDeclarations":[{"name":"read_file","parameters":` + params + `}]}],"contents":[{"role":"model","parts":[{"functionCall":{"name":"read_file","args":` + args + `}}]}]}`
			},
			unmarshal: func(b []byte) (*engine.ChatRequest, error) { return (&gemini.Adapter{}).Unmarshal(b) },
			marshal:   func(c *engine.ChatRequest) ([]byte, error) { return (&gemini.Adapter{}).Marshal(c) },
			extractArgs: func(t *testing.T, out []byte) []byte {
				var m struct {
					Contents []struct {
						Parts []struct {
							FunctionCall struct {
								Args json.RawMessage `json:"args"`
							} `json:"functionCall"`
						} `json:"parts"`
					} `json:"contents"`
				}
				if err := json.Unmarshal(out, &m); err != nil {
					t.Fatalf("extract: %v", err)
				}
				return m.Contents[0].Parts[0].FunctionCall.Args
			},
			extractParams: func(t *testing.T, out []byte) []byte {
				var m struct {
					Tools []struct {
						FunctionDeclarations []struct {
							Parameters json.RawMessage `json:"parameters"`
						} `json:"functionDeclarations"`
					} `json:"tools"`
				}
				if err := json.Unmarshal(out, &m); err != nil {
					t.Fatalf("extract: %v", err)
				}
				return m.Tools[0].FunctionDeclarations[0].Parameters
			},
		},
		{
			name: "bedrock",
			body: func(args, params string) string {
				return `{"modelId":"m","toolConfig":{"tools":[{"toolSpec":{"name":"read_file","inputSchema":{"json":` + params + `}}}]},"messages":[{"role":"assistant","content":[{"toolUse":{"toolUseId":"t1","name":"read_file","input":` + args + `}}]}]}`
			},
			unmarshal: func(b []byte) (*engine.ChatRequest, error) { return (&bedrock.Adapter{}).Unmarshal(b) },
			marshal:   func(c *engine.ChatRequest) ([]byte, error) { return (&bedrock.Adapter{}).Marshal(c) },
			extractArgs: func(t *testing.T, out []byte) []byte {
				var m struct {
					Messages []struct {
						Content []struct {
							ToolUse struct {
								Input json.RawMessage `json:"input"`
							} `json:"toolUse"`
						} `json:"content"`
					} `json:"messages"`
				}
				if err := json.Unmarshal(out, &m); err != nil {
					t.Fatalf("extract: %v", err)
				}
				return m.Messages[0].Content[0].ToolUse.Input
			},
			extractParams: func(t *testing.T, out []byte) []byte {
				var m struct {
					ToolConfig struct {
						Tools []struct {
							ToolSpec struct {
								InputSchema struct {
									JSON json.RawMessage `json:"json"`
								} `json:"inputSchema"`
							} `json:"toolSpec"`
						} `json:"tools"`
					} `json:"toolConfig"`
				}
				if err := json.Unmarshal(out, &m); err != nil {
					t.Fatalf("extract: %v", err)
				}
				return m.ToolConfig.Tools[0].ToolSpec.InputSchema.JSON
			},
		},
	}
	schema := `{"z":1,"properties":{"a":{"type":"string"},"m":{"type":"number"}},"required":["a"],"type":"object"}`
	for _, a := range arms {
		for _, c := range corpus {
			wantWireArgs := c.obj
			if !a.argsExactText {
				// RawMessage struct fields are compacted at the wire by
				// Go's Marshaler compaction; lexemes, key order, and
				// content survive.
				var buf bytes.Buffer
				if err := json.Compact(&buf, []byte(c.obj)); err != nil {
					t.Fatalf("compact: %v", err)
				}
				wantWireArgs = buf.String()
			}
			// Schemas are RawMessage struct fields on every arm (never a
			// string-embedded text), so they always compact.
			var buf bytes.Buffer
			if err := json.Compact(&buf, []byte(c.obj)); err != nil {
				t.Fatalf("compact: %v", err)
			}
			wantWireParams := buf.String()
			t.Run(a.name+"/args:"+c.name, func(t *testing.T) {
				chat, err := a.unmarshal([]byte(a.body(c.obj, schema)))
				if err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				got := toolCalls(chat.Messages[0])[0].Args.Bytes()
				if string(got) != c.obj {
					t.Fatalf("wrapper bytes = %q, want %q", got, c.obj)
				}
				out, err := a.marshal(chat)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				extracted := a.extractArgs(t, out)
				if string(extracted) != wantWireArgs {
					t.Fatalf("wire args = %q, want %q", extracted, wantWireArgs)
				}
			})
			t.Run(a.name+"/params:"+c.name, func(t *testing.T) {
				chat, err := a.unmarshal([]byte(a.body(`{"p":1}`, c.obj)))
				if err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				got := chat.Tools[0].Parameters.Bytes()
				if string(got) != c.obj {
					t.Fatalf("wrapper schema = %q, want %q", got, c.obj)
				}
				out, err := a.marshal(chat)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				extracted := a.extractParams(t, out)
				if string(extracted) != wantWireParams {
					t.Fatalf("wire schema = %q, want %q", extracted, wantWireParams)
				}
			})
		}
	}
}
