package bridge

import (
	"errors"
	"strings"
	"testing"
)

func projectRequestFixture(t *testing.T, from, to Protocol, body string) error {
	t.Helper()
	chat, err := ParseRequest(from, []byte(body), requestPath(from))
	if err != nil {
		return err
	}
	_, err = ProjectRequest(chat, from, to, RequestOptions{MaxTokens: 64, Project: "destination-project"})
	return err
}

func requireUnsupportedRequest(t *testing.T, err error) {
	t.Helper()
	var unsupportedErr *UnsupportedError
	if !errors.As(err, &unsupportedErr) {
		t.Fatalf("error = %v, want UnsupportedError", err)
	}
}

func TestRequestProjectionRejectsDuplicateFunctionDefinitions(t *testing.T) {
	tests := []struct {
		name string
		from Protocol
		to   Protocol
		body string
	}{
		{"chat", OpenAIChat, Anthropic, `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{}}},{"type":"function","function":{"name":"lookup","parameters":{}}}]}`},
		{"responses", OpenAIResponses, Anthropic, `{"input":"x","tools":[{"type":"function","name":"lookup","parameters":{}},{"type":"function","name":"lookup","parameters":{}}]}`},
		{"anthropic", Anthropic, OpenAIChat, `{"max_tokens":8,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"lookup","input_schema":{}},{"name":"lookup","input_schema":{}}]}`},
		{"gemini", Gemini, OpenAIChat, `{"contents":[{"role":"user","parts":[{"text":"x"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{}},{"name":"lookup","parameters":{}}]}]}`},
		{"codeassist", GeminiCodeAssist, OpenAIChat, `{"model":"m","project":"source-project","request":{"contents":[{"role":"user","parts":[{"text":"x"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{}},{"name":"lookup","parameters":{}}]}]}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := projectRequestFixture(t, tc.from, tc.to, tc.body)
			requireUnsupportedRequest(t, err)
			if err == nil || !strings.Contains(err.Error(), "duplicate function tool definitions") {
				t.Fatalf("duplicate definition error = %v", err)
			}
		})
	}
}

func TestRequestProjectionRejectsUnknownNamedToolChoice(t *testing.T) {
	tests := []struct {
		name string
		from Protocol
		to   Protocol
		body string
	}{
		{"chat", OpenAIChat, Anthropic, `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"other","parameters":{}}}],"tool_choice":{"type":"function","function":{"name":"missing"}}}`},
		{"responses", OpenAIResponses, Anthropic, `{"input":"x","tools":[{"type":"function","name":"other","parameters":{}}],"tool_choice":{"type":"function","name":"missing"}}`},
		{"anthropic", Anthropic, OpenAIChat, `{"max_tokens":8,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"other","input_schema":{}}],"tool_choice":{"type":"tool","name":"missing"}}`},
		{"gemini", Gemini, OpenAIChat, `{"contents":[{"role":"user","parts":[{"text":"x"}]}],"tools":[{"functionDeclarations":[{"name":"other","parameters":{}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["missing"]}}}`},
		{"codeassist", GeminiCodeAssist, OpenAIChat, `{"model":"m","project":"source-project","request":{"contents":[{"role":"user","parts":[{"text":"x"}]}],"tools":[{"functionDeclarations":[{"name":"other","parameters":{}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["missing"]}}}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := projectRequestFixture(t, tc.from, tc.to, tc.body)
			requireUnsupportedRequest(t, err)
			if err == nil || !strings.Contains(err.Error(), "named tool choice") {
				t.Fatalf("named choice error = %v", err)
			}
		})
	}
}

func TestRequestProjectionAcceptsNamedChoiceWithMatchingDefinition(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"lookup"}}}`
	for _, to := range []Protocol{OpenAIResponses, Anthropic, Gemini, GeminiCodeAssist} {
		t.Run(string(to), func(t *testing.T) {
			if err := projectRequestFixture(t, OpenAIChat, to, body); err != nil {
				t.Fatalf("matching named choice rejected: %v", err)
			}
		})
	}
}

func TestRequestProjectionValidatesDestinationToolIdentity(t *testing.T) {
	tests := []struct {
		name string
		from Protocol
		to   Protocol
		body string
	}{
		{"portable definition length", Gemini, OpenAIChat, `{"contents":[{"role":"user","parts":[{"text":"x"}]}],"tools":[{"functionDeclarations":[{"name":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","parameters":{}}]}]}`},
		{"gemini definition leading digit", OpenAIChat, Gemini, `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"1lookup","parameters":{}}}]}`},
		{"gemini call leading digit", OpenAIChat, Gemini, `{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"1lookup","arguments":"{}"}}]}]}`},
		{"anthropic call id punctuation", OpenAIChat, Anthropic, `{"messages":[{"role":"assistant","tool_calls":[{"id":"call.with/slash","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := projectRequestFixture(t, tc.from, tc.to, tc.body)
			requireUnsupportedRequest(t, err)
		})
	}
}

func TestCodeAssistSourceEnvelopeModelAndProjectAreRecognized(t *testing.T) {
	body := requestFixtures[GeminiCodeAssist]
	for _, to := range []Protocol{OpenAIChat, OpenAIResponses, Anthropic, Gemini} {
		t.Run(string(to), func(t *testing.T) {
			if err := projectRequestFixture(t, GeminiCodeAssist, to, body); err != nil {
				t.Fatalf("ordinary Code Assist model/project envelope rejected: %v", err)
			}
		})
	}
}

func TestParseRequestStrictJSONBeforeAdapterDecode(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{"duplicate top member", []byte(`{"messages":[{"role":"user","content":"first"}],"messages":[{"role":"user","content":"secret"}]}`)},
		{"duplicate nested member", []byte(`{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"first","name":"secret","parameters":{}}}]}`)},
		{"lone surrogate", []byte(`{"messages":[{"role":"user","content":"\ud800"}]}`)},
		{"invalid utf8", append([]byte(`{"messages":[{"role":"user","content":"`), append([]byte{0xff}, []byte(`"}]}`)...)...)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRequest(OpenAIChat, tc.body, requestPath(OpenAIChat))
			requireUnsupportedRequest(t, err)
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("strict parse error = %v", err)
			}
		})
	}
}
