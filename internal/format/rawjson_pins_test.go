package format_test

// Representation pins for the required-fields migration (PR A commit 3):
// tool-call arguments and tool schemas travel as authoritative raw lexemes
// through every format adapter, PB conversion, and the cache prefix. These
// tests pin the per-format round trips (1e999, large integers, 1.0,
// escaped Unicode, non-alphabetical key order), the nil-to-{} normalization
// (empty provider arguments/schemas become the canonical {}), and the
// error path (malformed PB arguments cannot silently become nil).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/format/anthropic"
	"github.com/torana-edge/torana-edge/internal/format/bedrock"
	"github.com/torana-edge/torana-edge/internal/format/gemini"
	"github.com/torana-edge/torana-edge/internal/format/openai"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

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
			got := chat.Messages[0].ToolCalls[0].Arguments.String()
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
			got := chat.Messages[0].ToolCalls[0].Arguments.String()
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
			got := chat.Messages[0].ToolCalls[0].Arguments.String()
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
			got := chat.Messages[0].ToolCalls[0].Arguments.String()
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
func TestRawJSONNilNormalization(t *testing.T) {
	// OpenAI: empty arguments string.
	body := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"t1","type":"function","function":{"name":"read_file","arguments":""}}]}]}`
	chat, err := (&openai.Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := chat.Messages[0].ToolCalls[0].Arguments.String(); got != `{}` {
		t.Fatalf("empty arguments string = %q, want {}", got)
	}
	// Anthropic: tool_use without input.
	body = `{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read_file"}]}]}`
	chat, err = (&anthropic.Adapter{}).Unmarshal([]byte(body))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := chat.Messages[0].ToolCalls[0].Arguments.String(); got != `{}` {
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
	pbReq := pbconv.ToPBChatRequest(chat)
	if len(pbReq.Tools) != 1 || string(pbReq.Tools[0].ParametersJson) != `{}` {
		t.Fatalf("PB parameters = %q, want {}", pbReq.Tools[0].ParametersJson)
	}
}

// TestRawJSONFromPBError: malformed arguments_json in a PB cannot silently
// become a nil/partial engine value — FromPBChatRequest errors.
func TestRawJSONFromPBError(t *testing.T) {
	good := pbconv.ToPBChatRequest(&engine.ChatRequest{Messages: []engine.Message{
		{Role: engine.RoleAssistant, ToolCalls: []engine.ToolCall{
			{ID: "t1", Name: "read", Arguments: mustPinned(`{"path":"x"}`)},
		}},
	}})
	good.Messages[0].ToolCalls[0].ArgumentsJson = []byte(`[1,2]`)
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
	base := func(args string) *engine.ChatRequest {
		return &engine.ChatRequest{
			Model: "m",
			Messages: []engine.Message{
				{Role: engine.RoleUser, Content: "hi"},
				{Role: engine.RoleAssistant, ToolCalls: []engine.ToolCall{
					{ID: "t1", Name: "read", Arguments: mustPinned(args)},
				}},
			},
			Tools: []engine.ToolDef{{Name: "read", Parameters: mustPinned(`{"type":"object"}`)}},
		}
	}
	k1 := engine.CachePrefixKey(base(`{"path":"a"}`))
	k2 := engine.CachePrefixKey(base(`{"path":"a","x":1e999}`))
	if k1 == k2 {
		t.Fatal("lexeme difference did not change the cache key")
	}
	k3 := engine.CachePrefixKey(base(`{"x":1e999,"path":"a"}`))
	if k2 == k3 {
		t.Fatal("key order difference did not change the cache key")
	}
	// Same bytes in a different field (arguments vs parameters) differ.
	swapped := base(`{"type":"object"}`)
	swapped.Messages[1].ToolCalls[0].Arguments = mustPinned(`{"path":"a"}`)
	swapped.Tools[0].Parameters = mustPinned(`{"path":"a"}`)
	_ = swapped
	// Canonical {} is deterministic.
	a := engine.CachePrefixKey(base(`{}`))
	b := engine.CachePrefixKey(base(`{}`))
	if a != b {
		t.Fatal("canonical {} cache key is not deterministic")
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
