package openai

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func TestResponsesInstructionsCanonicalTopologyAndInputAlignment(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","instructions":"top-level","input":[
		{"vendor":{"z":1e999,"a":1.0},"role":"system","content":[{"text":"input-system-1","type":"input_text"}],"type":"message"},
		{"type":"reasoning","encrypted_content":"opaque"},
		{"role":"system","content":[{"type":"input_text","text":"input-system-2"}],"type":"message"},
		{"arguments":"{\"z\":1,\"a\":2}","name":"f","call_id":"c1","type":"function_call"}
	]}`)

	req, err := (&Adapter{}).Unmarshal(raw)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !req.ResponsesInstructions {
		t.Fatal("present instructions topology was not captured")
	}
	if len(req.Messages) != 4 {
		t.Fatalf("canonical messages = %d, want instructions + 3 representable input items", len(req.Messages))
	}
	for i, want := range []string{"top-level", "input-system-1", "input-system-2"} {
		if req.Messages[i].Role != engine.RoleSystem || len(req.Messages[i].Blocks) != 1 ||
			req.Messages[i].Blocks[0].Text == nil || req.Messages[i].Blocks[0].Text.Text != want {
			t.Fatalf("system message %d = %#v, want %q", i, req.Messages[i], want)
		}
	}

	out, err := (&Adapter{}).Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var envelope struct {
		Instructions *string           `json:"instructions"`
		Input        []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Instructions == nil || *envelope.Instructions != "top-level" {
		t.Fatalf("instructions = %v, want top-level", envelope.Instructions)
	}
	if len(envelope.Input) != 4 {
		t.Fatalf("input items = %d, want original 4 slots", len(envelope.Input))
	}
	for i := range envelope.Input {
		want := mustInputItem(t, raw, i)
		var wantCompact, gotCompact bytes.Buffer
		if err := json.Compact(&wantCompact, want); err != nil {
			t.Fatal(err)
		}
		if err := json.Compact(&gotCompact, envelope.Input[i]); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotCompact.Bytes(), wantCompact.Bytes()) {
			t.Fatalf("input item %d changed or moved:\n got %s\nwant %s", i, gotCompact.Bytes(), wantCompact.Bytes())
		}
	}
}

func TestResponsesInstructionsPresentEmptyVersusAbsent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		present bool
	}{
		{name: "present empty", raw: `{"model":"m","instructions":"","input":"hi"}`, present: true},
		{name: "absent", raw: `{"model":"m","input":"hi"}`, present: false},
		{name: "null is absent", raw: `{"model":"m","instructions":null,"input":"hi"}`, present: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := (&Adapter{}).Unmarshal([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if req.ResponsesInstructions != tc.present {
				t.Fatalf("ResponsesInstructions = %v, want %v", req.ResponsesInstructions, tc.present)
			}
			if tc.present {
				if len(req.Messages) != 2 || req.Messages[0].Blocks[0].Text == nil || req.Messages[0].Blocks[0].Text.Text != "" {
					t.Fatalf("empty instructions were not a first-class canonical text message: %#v", req.Messages)
				}
			} else if len(req.Messages) != 1 || req.Messages[0].Role != engine.RoleUser {
				t.Fatalf("absent instructions invented a message: %#v", req.Messages)
			}

			out, err := (&Adapter{}).Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			var members map[string]json.RawMessage
			if err := json.Unmarshal(out, &members); err != nil {
				t.Fatal(err)
			}
			_, gotPresent := members["instructions"]
			if gotPresent != tc.present {
				t.Fatalf("wire instructions presence = %v, want %v: %s", gotPresent, tc.present, out)
			}
			if tc.present && string(members["instructions"]) != `""` {
				t.Fatalf("empty instructions changed: %s", members["instructions"])
			}
		})
	}
}

func TestResponsesInstructionsReplacementTopologyValidation(t *testing.T) {
	plain := func(text string) *pb.RequestBlock {
		return &pb.RequestBlock{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: text}}}
	}
	for _, tc := range []struct {
		name string
		req  *pb.ChatRequest
		ok   bool
	}{
		{name: "text replacement", req: &pb.ChatRequest{Messages: []*pb.Message{{Role: "system", Blocks: []*pb.RequestBlock{plain("changed")}}}}, ok: true},
		{name: "removed", req: &pb.ChatRequest{}, ok: false},
		{name: "role changed", req: &pb.ChatRequest{Messages: []*pb.Message{{Role: "user", Blocks: []*pb.RequestBlock{plain("changed")}}}}, ok: false},
		{name: "second block", req: &pb.ChatRequest{Messages: []*pb.Message{{Role: "system", Blocks: []*pb.RequestBlock{plain("a"), plain("b")}}}}, ok: false},
		{name: "metadata", req: &pb.ChatRequest{Messages: []*pb.Message{{Role: "system", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "changed", PartMetadataJson: []byte(`{}`)}}}}}}}, ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifyResponsesInstructionsTopologyPB(true, tc.req) == nil; got != tc.ok {
				t.Fatalf("accepted = %v, want %v", got, tc.ok)
			}
		})
	}
	if err := VerifyResponsesInstructionsTopologyPB(false, &pb.ChatRequest{}); err != nil {
		t.Fatalf("absent instructions topology constrained replacement: %v", err)
	}
	for _, key := range []string{"instructions", "max_output_tokens", "temperature", "top_p"} {
		t.Run("extension smuggling "+key, func(t *testing.T) {
			req := &pb.ChatRequest{ProviderExtensionsJson: []byte(`{"` + key + `":1}`)}
			if err := VerifyResponsesInstructionsTopologyPB(false, req); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("error = %v, want canonical-member refusal", err)
			}
		})
	}
}

func TestResponsesMarshalRejectsCanonicalExtensionSmuggling(t *testing.T) {
	for _, key := range []string{"instructions", "max_output_tokens", "temperature", "top_p"} {
		t.Run(key, func(t *testing.T) {
			ext, err := engine.ParseOptionalJSONObject([]byte(`{"` + key + `":1}`))
			if err != nil {
				t.Fatal(err)
			}
			req := &engine.ChatRequest{
				Model: "m", OpenAIVariant: engine.OpenAIResponses, ProviderExtensions: ext,
				Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}}},
			}
			if _, err := (&Adapter{}).Marshal(req); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("error = %v, want canonical-member refusal", err)
			}
		})
	}
}

func TestResponsesParametersUseCanonicalFields(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5",
		"input":"hello",
		"max_output_tokens":321,
		"temperature":0.25,
		"top_p":0.75,
		"future_parameter":{"n":9007199254740993}
	}`)

	req, err := (&Adapter{}).Unmarshal(raw)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 321 {
		t.Fatalf("MaxTokens = %v, want 321", req.MaxTokens)
	}
	if req.Temperature == nil || *req.Temperature != 0.25 {
		t.Fatalf("Temperature = %v, want 0.25", req.Temperature)
	}
	if req.TopP == nil || *req.TopP != 0.75 {
		t.Fatalf("TopP = %v, want 0.75", req.TopP)
	}

	var extensions map[string]json.RawMessage
	if err := json.Unmarshal(req.ProviderExtensions.Bytes(), &extensions); err != nil {
		t.Fatalf("provider extensions: %v", err)
	}
	for _, name := range []string{"max_output_tokens", "temperature", "top_p"} {
		if _, exists := extensions[name]; exists {
			t.Errorf("canonical field %q remained in provider extensions", name)
		}
	}
	if got := string(extensions["future_parameter"]); got != `{"n":9007199254740993}` {
		t.Fatalf("future_parameter = %s, want lexeme-preserved object", got)
	}

	maxTokens := 654
	temperature := 0.5
	topP := 0.9
	req.MaxTokens = &maxTokens
	req.Temperature = &temperature
	req.TopP = &topP
	out, err := (&Adapter{}).Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatalf("wire JSON: %v", err)
	}
	if string(wire["max_output_tokens"]) != "654" {
		t.Errorf("max_output_tokens = %s, want 654", wire["max_output_tokens"])
	}
	if string(wire["temperature"]) != "0.5" {
		t.Errorf("temperature = %s, want 0.5", wire["temperature"])
	}
	if string(wire["top_p"]) != "0.9" {
		t.Errorf("top_p = %s, want 0.9", wire["top_p"])
	}
	if _, exists := wire["max_tokens"]; exists {
		t.Error("Responses request emitted Chat Completions max_tokens")
	}
}

func TestResponsesMaxOutputTokensAcceptedDomain(t *testing.T) {
	for _, value := range []string{"0", "-1", "2147483648"} {
		t.Run(value, func(t *testing.T) {
			_, err := (&Adapter{}).Unmarshal([]byte(`{"model":"m","input":"hi","max_output_tokens":` + value + `}`))
			if err == nil || !strings.Contains(err.Error(), "max_output_tokens") {
				t.Fatalf("error = %v, want max_output_tokens domain error", err)
			}
		})
	}

	maxTokens := int(math.MaxInt32)
	req, err := (&Adapter{}).Unmarshal([]byte(`{"model":"m","input":"hi","max_output_tokens":2147483647}`))
	if err != nil {
		t.Fatalf("maximum accepted value refused: %v", err)
	}
	if req.MaxTokens == nil || *req.MaxTokens != maxTokens {
		t.Fatalf("MaxTokens = %v, want %d", req.MaxTokens, maxTokens)
	}
}

func TestResponsesNullParametersRemainAbsent(t *testing.T) {
	req, err := (&Adapter{}).Unmarshal([]byte(`{
		"model":"m","input":"hi",
		"max_output_tokens":null,"temperature":null,"top_p":null
	}`))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if req.MaxTokens != nil || req.Temperature != nil || req.TopP != nil {
		t.Fatalf("null canonical parameters became present: max=%v temperature=%v top_p=%v",
			req.MaxTokens, req.Temperature, req.TopP)
	}
	out, err := (&Adapter{}).Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, name := range []string{"max_output_tokens", "temperature", "top_p"} {
		if strings.Contains(string(out), `"`+name+`"`) {
			t.Errorf("null/absent %q was invented: %s", name, out)
		}
	}
}

func TestResponsesDoesNotAliasChatMaxCompletionTokens(t *testing.T) {
	req, err := (&Adapter{}).Unmarshal([]byte(`{
		"model":"m","input":"hi","max_completion_tokens":777
	}`))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if req.MaxTokens != nil {
		t.Fatalf("Responses max_completion_tokens populated MaxTokens: %v", req.MaxTokens)
	}
	var extensions map[string]json.RawMessage
	if err := json.Unmarshal(req.ProviderExtensions.Bytes(), &extensions); err != nil {
		t.Fatalf("provider extensions: %v", err)
	}
	if string(extensions["max_completion_tokens"]) != "777" {
		t.Fatalf("max_completion_tokens extension = %s, want 777", extensions["max_completion_tokens"])
	}
}
