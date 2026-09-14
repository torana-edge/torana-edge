package bridge

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestGeminiBridgeSchemaDialectConversion(t *testing.T) {
	raw := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"OBJECT","properties":{"count":{"type":"INTEGER","minimum":9007199254740993},"items":{"type":"ARRAY","maxItems":"5","items":{"type":"STRING"},"nullable":true}},"required":["count"]}}]}]}`
	req, err := ParseRequest(Gemini, []byte(raw), requestPath(Gemini))
	if err != nil {
		t.Fatal(err)
	}
	params := req.Tools[0].Parameters.Bytes()
	for _, want := range []string{`"type":"object"`, `"type":"integer"`, `"type":"null"`, `"maxItems":5`, `9007199254740993`} {
		if !bytes.Contains(params, []byte(want)) {
			t.Fatalf("missing %s in canonical schema %s", want, params)
		}
	}
	out, err := ProjectRequest(req, Gemini, OpenAIChat, RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := MarshalRequest(OpenAIChat, out)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte(`"OBJECT"`)) {
		t.Fatalf("Gemini type enum leaked: %s", wire)
	}
}

func TestGeminiBridgeUsesJSONSchemaField(t *testing.T) {
	raw := `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","additionalProperties":false,"properties":{"n":{"type":"integer","const":9007199254740993}}}}}]}`
	req, err := ParseRequest(OpenAIChat, []byte(raw), requestPath(OpenAIChat))
	if err != nil {
		t.Fatal(err)
	}
	out, err := ProjectRequest(req, OpenAIChat, Gemini, RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := MarshalRequest(Gemini, out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(wire, []byte(`"parametersJsonSchema"`)) || bytes.Contains(wire, []byte(`"parameters":`)) {
		t.Fatalf("wrong Gemini schema field: %s", wire)
	}
	back, err := ParseRequest(Gemini, wire, requestPath(Gemini))
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	_ = json.Unmarshal(out.Tools[0].Parameters.Bytes(), &a)
	_ = json.Unmarshal(back.Tools[0].Parameters.Bytes(), &b)
	ac, _ := json.Marshal(a)
	bc, _ := json.Marshal(b)
	if !bytes.Equal(ac, bc) || !bytes.Contains(back.Tools[0].Parameters.Bytes(), []byte("9007199254740993")) {
		t.Fatalf("schema changed: %s", back.Tools[0].Parameters.Bytes())
	}
	if _, err = ProjectRequest(req, OpenAIChat, GeminiCodeAssist, RequestOptions{Project: "p"}); err == nil {
		t.Fatal("silently projected unsupported JSONSchema constraints to Code Assist Schema")
	}
}

func TestGeminiBridgeRefusesConflictingSchemaDialects(t *testing.T) {
	raw := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"OBJECT"},"parametersJsonSchema":{"type":"object"}}]}]}`
	if _, err := ParseRequest(Gemini, []byte(raw), requestPath(Gemini)); err == nil {
		t.Fatal("ambiguous schemas accepted")
	}
}
