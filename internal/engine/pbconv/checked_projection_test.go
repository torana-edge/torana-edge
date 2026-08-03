package pbconv_test

import (
	"math"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/format/anthropic"
	"github.com/torana-edge/torana-edge/internal/format/bedrock"
	"github.com/torana-edge/torana-edge/internal/format/gemini"
	"github.com/torana-edge/torana-edge/internal/format/openai"
)

// The accepted-input closure (review finding 2): the Engine->PB boundary is
// a CHECKED projection. The engine block representation is a pointer sum,
// so zero/multi-arm states, nested conflicts, non-finite floats, and
// out-of-range max tokens must be refused HERE — the protobuf oneof cannot
// see facts the conversion already discarded.
//
// These rows were written BEFORE the checked boundary existed.

func validChat() *engine.ChatRequest {
	return &engine.ChatRequest{
		Model:     "m",
		MaxTokens: new(1024),
		Messages: []engine.Message{
			{Role: engine.RoleSystem, Blocks: []engine.Block{
				{Text: &engine.TextBlock{Text: "sys"}},
			}},
			{Role: engine.RoleUser, Blocks: []engine.Block{
				{Text: &engine.TextBlock{Text: "hi"}},
				{ToolUse: &engine.ToolUseBlock{ID: "c1", Name: "read", Arguments: mustReqObj(`{"p":1}`)}},
			}},
			{Role: engine.RoleUser, Blocks: []engine.Block{
				{ToolResult: &engine.ToolResultBlock{
					ToolCallID: "c1",
					Content:    []engine.ToolResultContentBlock{{Text: "out"}},
				}},
			}},
		},
		Tools: []engine.ToolDef{{Name: "read", Parameters: mustReqObj(`{"type":"object"}`)}},
	}
}

func mustReqObj(raw string) engine.RequiredJSONObject {
	r, err := engine.ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		panic(err)
	}
	return r
}

// TestCheckedProjectionRefusesEngineInvalidStates — every engine state the
// protobuf oneof cannot carry is refused at the checked boundary.
func TestCheckedProjectionRefusesEngineInvalidStates(t *testing.T) {
	cases := map[string]func(*engine.ChatRequest){
		"zero-arm block": func(c *engine.ChatRequest) {
			c.Messages[0].Blocks = []engine.Block{{}}
		},
		"multi-arm block": func(c *engine.ChatRequest) {
			c.Messages[0].Blocks = []engine.Block{{
				Text:    &engine.TextBlock{Text: "a"},
				ToolUse: &engine.ToolUseBlock{ID: "c1", Name: "read", Arguments: mustReqObj(`{}`)},
			}}
		},
		"nested text+unknown conflict": func(c *engine.ChatRequest) {
			c.Messages[2].Blocks[0].ToolResult.Content = []engine.ToolResultContentBlock{{
				Text:    "a",
				Unknown: &engine.UnknownBlock{Kind: "image", Payload: mustReqObj(`{}`)},
			}}
		},
		"nested text+cache conflict": func(c *engine.ChatRequest) {
			c.Messages[2].Blocks[0].ToolResult.Content = []engine.ToolResultContentBlock{{
				Text: "a",
				CacheBreakpoint: &engine.CacheBreakpointBlock{
					Marker: mustReqObj(`{"type":"ephemeral"}`),
				},
			}}
		},
		"nested unknown+cache conflict": func(c *engine.ChatRequest) {
			c.Messages[2].Blocks[0].ToolResult.Content = []engine.ToolResultContentBlock{{
				Unknown: &engine.UnknownBlock{Kind: "image", Payload: mustReqObj(`{}`)},
				CacheBreakpoint: &engine.CacheBreakpointBlock{
					Marker: mustReqObj(`{"type":"ephemeral"}`),
				},
			}}
		},
		"empty nested content": func(c *engine.ChatRequest) {
			c.Messages[2].Blocks[0].ToolResult.Content = nil
		},
		"max tokens above MaxInt32": func(c *engine.ChatRequest) {
			c.MaxTokens = new(math.MaxInt32 + 1)
		},
		"max tokens below 1": func(c *engine.ChatRequest) {
			c.MaxTokens = new(0)
		},
		"NaN temperature": func(c *engine.ChatRequest) {
			c.Temperature = new(math.NaN())
		},
		"infinite top_p": func(c *engine.ChatRequest) {
			c.TopP = new(math.Inf(1))
		},
		"empty tool name": func(c *engine.ChatRequest) {
			c.Tools[0].Name = ""
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := validChat()
			mutate(c)
			if _, err := pbconv.ToPBChatRequestChecked(c); err == nil {
				t.Fatalf("invalid engine state %q passed the checked boundary", name)
			}
		})
	}
}

// TestCheckedProjectionAcceptsValidCorpus — every valid shape (all block
// kinds, signatures, nested content, cache carriers, extensions) passes the
// checked boundary AND the SDK validator agrees on the resulting PB.
func TestCheckedProjectionAcceptsValidCorpus(t *testing.T) {
	chat := &engine.ChatRequest{
		Model:              "m",
		MaxTokens:          new(4096),
		Temperature:        new(0.7),
		TopP:               new(0.9),
		StopSequences:      []string{"\n\n"},
		ProviderExtensions: mustOptObj(`{"extra":1}`),
		SafetySettings:     mustOptArr(`[{"category":"HARM"}]`),
		Messages: []engine.Message{
			{Role: engine.RoleSystem, Blocks: []engine.Block{
				{Text: &engine.TextBlock{Text: "sys"}},
				{CacheBreakpoint: &engine.CacheBreakpointBlock{Marker: mustReqObj(`{"type":"ephemeral"}`)}},
			}},
			{Role: engine.RoleAssistant, Blocks: []engine.Block{
				{Thinking: &engine.ThinkingBlock{Text: "reasoning", Signature: "TS"}},
				{Text: &engine.TextBlock{Text: "answer", Signature: "CS"}},
				{ToolUse: &engine.ToolUseBlock{ID: "c1", Name: "read", Arguments: mustReqObj(`{"p":1}`), Signature: "CALLS"}},
				{TrailingSignature: &engine.TrailingSignatureBlock{Signature: "TRAIL"}},
			}},
			{Role: engine.RoleUser, Blocks: []engine.Block{
				{ToolResult: &engine.ToolResultBlock{
					ToolCallID: "c1",
					Content: []engine.ToolResultContentBlock{
						{Text: "a"},
						{Unknown: &engine.UnknownBlock{Kind: "image", Payload: mustReqObj(`{"src":"x"}`)}},
						{CacheBreakpoint: &engine.CacheBreakpointBlock{Marker: mustReqObj(`{"type":"ephemeral"}`)}},
					},
				}},
				{Unknown: &engine.UnknownBlock{Kind: "future_arm", Payload: mustReqObj(`{"x":1}`)}},
			}},
		},
		Tools: []engine.ToolDef{
			{Name: "read", Parameters: mustReqObj(`{"type":"object"}`), CacheControl: mustOptObj(`{"type":"ephemeral"}`)},
		},
	}
	pbReq, err := pbconv.ToPBChatRequestChecked(chat)
	if err != nil {
		t.Fatalf("valid corpus refused at the checked boundary: %v", err)
	}
	if err := pbReq.ValidateReplacement(); err != nil {
		t.Fatalf("checked conversion output failed the SDK validator: %v", err)
	}
}

func mustOptObj(raw string) engine.OptionalJSONObject {
	r, err := engine.ParseOptionalJSONObject([]byte(raw))
	if err != nil {
		panic(err)
	}
	return r
}

func mustOptArr(raw string) engine.OptionalJSONArray {
	r, err := engine.ParseOptionalJSONArray([]byte(raw))
	if err != nil {
		panic(err)
	}
	return r
}

// TestCheckedProjectionAcceptsAdapterOutputs — every adapter's accepted
// parse output passes the checked boundary and the SDK validator (the
// cross-format acceptance closure).
func TestCheckedProjectionAcceptsAdapterOutputs(t *testing.T) {
	bodies := []struct {
		name string
		body string
	}{
		{"anthropic", `{"model":"m","max_tokens":1024,"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"read","input":{"p":1}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"out"}]}]}`},
		{"bedrock", `{"modelId":"m","system":[{"text":"sys"}],"messages":[{"role":"user","content":[{"text":"hi"}]},{"role":"assistant","content":[{"toolUse":{"toolUseId":"c1","name":"read","input":{"p":1}}}]},{"role":"user","content":[{"toolResult":{"toolUseId":"c1","content":[{"text":"out"}]}}]}]}`},
		{"gemini", `{"model":"m","systemInstruction":{"parts":[{"text":"sys"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"functionCall":{"name":"read","args":{"p":1},"id":"c1"}}]},{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"out"},"id":"c1"}}]}]}`},
		{"openai chat", `{"model":"m","max_tokens":1024,"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"p\":1}"}}]},{"role":"tool","tool_call_id":"c1","content":"out"}]}`},
	}
	for _, tc := range bodies {
		t.Run(tc.name, func(t *testing.T) {
			chat, err := adapterFor(tc.name).Unmarshal([]byte(tc.body))
			if err != nil {
				t.Fatalf("adapter parse: %v", err)
			}
			pbReq, err := pbconv.ToPBChatRequestChecked(chat)
			if err != nil {
				t.Fatalf("accepted adapter output refused at the checked boundary: %v", err)
			}
			if err := pbReq.ValidateReplacement(); err != nil {
				t.Fatalf("adapter output failed the SDK validator: %v", err)
			}
		})
	}
}

type adapterAPI interface {
	Unmarshal([]byte) (*engine.ChatRequest, error)
	Marshal(*engine.ChatRequest) ([]byte, error)
}

func adapterFor(name string) adapterAPI {
	switch name {
	case "anthropic":
		return &anthropic.Adapter{}
	case "bedrock":
		return &bedrock.Adapter{}
	case "gemini":
		return &gemini.Adapter{}
	default:
		return &openai.Adapter{}
	}
}

// TestCheckedProjectionErrorNamesTheState — the refusal is specific, not a
// blanket conversion failure.
func TestCheckedProjectionErrorNamesTheState(t *testing.T) {
	c := validChat()
	c.Messages[0].Blocks = []engine.Block{{}}
	_, err := pbconv.ToPBChatRequestChecked(c)
	if err == nil {
		t.Fatal("zero-arm block accepted")
	}
	if !strings.Contains(err.Error(), "zero") && !strings.Contains(err.Error(), "arm") {
		t.Fatalf("error = %q, want the arm state named", err)
	}
}

// TestSharedDomainTable — the accepted-input and replacement-output sides
// share ONE table: a whitespace-only tool name is accepted by BOTH
// ValidateEngineRequest and the SDK's ValidateReplacement (the SDK rejects
// only name == ""), so the host never rejects what a plugin may return.
func TestSharedDomainTable(t *testing.T) {
	c := validChat()
	c.Tools[0].Name = "   "
	pbReq, err := pbconv.ToPBChatRequestChecked(c)
	if err != nil {
		t.Fatalf("whitespace-only tool name refused on the accepted side: %v", err)
	}
	if err := pbReq.ValidateReplacement(); err != nil {
		t.Fatalf("whitespace-only tool name refused on the replacement side: %v", err)
	}
	// The empty-name refusal is the shared rule on both sides.
	c.Tools[0].Name = ""
	if _, err := pbconv.ToPBChatRequestChecked(c); err == nil {
		t.Fatal("empty tool name accepted on the accepted side")
	}
}

// TestAdapterMarshalEntryValidates — every provider Marshal entry point runs
// the owning validation: a multi-arm engine request is refused at marshal,
// so no future call site can bypass the checked boundary by calling an
// adapter directly.
func TestAdapterMarshalEntryValidates(t *testing.T) {
	multiArm := &engine.ChatRequest{
		Model: "m",
		Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{
			{Text: &engine.TextBlock{Text: "a"}, ToolUse: &engine.ToolUseBlock{ID: "c1", Name: "r", Arguments: mustReqObj(`{}`)}},
		}}},
	}
	for _, name := range []string{"anthropic", "bedrock", "gemini", "openai"} {
		t.Run(name, func(t *testing.T) {
			if _, err := adapterFor(name).Marshal(multiArm); err == nil {
				t.Fatalf("%s marshal accepted a multi-arm engine request", name)
			}
		})
	}
	// A valid request still marshals.
	simple := &engine.ChatRequest{
		Model:    "m",
		Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}}},
	}
	for _, name := range []string{"anthropic", "bedrock", "gemini", "openai"} {
		ok, err := adapterFor(name).Marshal(simple)
		if err != nil || len(ok) == 0 {
			t.Fatalf("%s: valid request failed to marshal: %v", name, err)
		}
	}
}
