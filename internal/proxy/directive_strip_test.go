package proxy

import (
	"bytes"
	"testing"

	"github.com/torana-edge/torana-edge/internal/bridge"
	"github.com/torana-edge/torana-edge/internal/format"
)

func TestStripDirectiveTextPreservesUntouchedJSONForFiveShapes(t *testing.T) {
	cases := []struct {
		name, shape, body, want string
	}{
		{
			"chat", "openai",
			`{ "model":"m", "messages":[{"role":"user","content":"hello\ntorana> status\nworld"}], "opaque":{"large":9007199254740993} }`,
			`{ "model":"m", "messages":[{"role":"user","content":"hello\nworld"}], "opaque":{"large":9007199254740993} }`,
		},
		{
			"responses", "openai-responses",
			`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi\ntorana> help"}]}],"opaque":"a\\/b"}`,
			`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi\n"}]}],"opaque":"a\\/b"}`,
		},
		{
			"anthropic", "anthropic",
			`{"messages":[{"role":"user","content":[{"type":"text","text":"torana> accept 7f3k\nnext"}]}],"max_tokens":64,"opaque":1.00}`,
			`{"messages":[{"role":"user","content":[{"type":"text","text":"next"}]}],"max_tokens":64,"opaque":1.00}`,
		},
		{
			"gemini", "gemini",
			`{"contents":[{"role":"user","parts":[{"text":"torana> status\nhello"}]}],"unknown":{"a":1e999}}`,
			`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"unknown":{"a":1e999}}`,
		},
		{
			"code-assist", "gemini-codeassist",
			`{"project":"p","request":{"contents":[{"role":"user","parts":[{"text":"torana> help\nhello"}]}],"opaque":true}}`,
			`{"project":"p","request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"opaque":true}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, commands, changed, err := stripDirectiveText([]byte(tc.body), tc.shape, nil)
			if err != nil || !changed || len(commands) != 1 || string(got) != tc.want {
				t.Fatalf("strip = %s, commands %+v, changed %v, error %v; want %s", got, commands, changed, err, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		shape, path, body string
		protocol          bridge.Protocol
	}{
		{"openai-responses", "/v1/responses", `{"model":"m","input":"torana> status"}`, bridge.OpenAIResponses},
		{"gemini-codeassist", "/v1/models/m:generateContent", `{"request":{"contents":[{"role":"user","parts":[{"text":"torana> status"}]}]},"model":"m"}`, bridge.GeminiCodeAssist},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			body, commands, _, err := stripDirectiveText([]byte(tc.body), tc.shape, nil)
			if err != nil || len(commands) != 1 {
				t.Fatalf("strip commands=%+v err=%v", commands, err)
			}
			if _, err := bridge.ParseRequest(tc.protocol, body, tc.path); err != nil {
				t.Fatalf("current local turn became unparseable: %v", err)
			}
		})
	}
}

func TestStripDirectiveTextDoesNotTouchToolResultsOrPlainRequests(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"tool","content":"torana> accept 7f3k"}]}`)
	got, commands, changed, err := stripDirectiveText(body, "openai", nil)
	if err != nil || changed || len(commands) != 0 || !bytes.Equal(got, body) {
		t.Fatalf("plain/tool request changed: %s, %+v, %v, %v", got, commands, changed, err)
	}
	mixed := []byte(`{"messages":[{"role":"user","content":"torana> status"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"result"},{"type":"text","text":"torana> help"}]}]}`)
	got, commands, changed, err = stripDirectiveText(mixed, "anthropic", nil)
	if err != nil || !changed || len(commands) != 0 || bytes.Contains(got, []byte(`torana> help`)) {
		t.Fatalf("mixed tool continuation: %s, %+v, %v, %v", got, commands, changed, err)
	}
	replayed := []byte(`{"messages":[{"role":"user","content":"torana> accept 7f3k"},{"role":"assistant","content":"calling"},{"role":"tool","content":"done"}]}`)
	_, commands, changed, err = stripDirectiveText(replayed, "openai", nil)
	if err != nil || !changed || len(commands) != 0 {
		t.Fatalf("replayed command executed on tool continuation: %+v, %v, %v", commands, changed, err)
	}
}

func TestStripDirectiveOnlyHistoricalUserItem(t *testing.T) {
	cases := []struct {
		shape, body, want string
	}{
		{"openai-chat", `{"messages":[{"role":"user","content":"torana> status"},{"role":"assistant","content":"local"},{"role":"user","content":"next"}]}`, `{"messages":[{"role":"assistant","content":"local"},{"role":"user","content":"next"}]}`},
		{"openai-chat", `{"messages":[{"role":"user","content":"torana> status\nplease do something else"},{"role":"assistant","content":"local"},{"role":"user","content":"next"}]}`, `{"messages":[{"role":"assistant","content":"local"},{"role":"user","content":"next"}]}`},
		{"openai-chat", `{"messages":[{"role":"user","content":"torana> status"},{"role":"assistant","content":"local"},{"role":"user","content":"next"}],"opaque":1e999}`, `{"messages":[{"role":"assistant","content":"local"},{"role":"user","content":"next"}],"opaque":1e999}`},
		{"anthropic", `{"messages":[{"role":"user","content":[{"type":"text","text":"torana> status"}]},{"role":"assistant","content":"local"},{"role":"user","content":"next"}]}`, `{"messages":[{"role":"assistant","content":"local"},{"role":"user","content":"next"}]}`},
		{"openai-responses", `{"input":[{"role":"user","content":[{"type":"input_text","text":"torana> status"}]},{"role":"assistant","content":"local"},{"role":"user","content":"next"}]}`, `{"input":[{"role":"assistant","content":"local"},{"role":"user","content":"next"}]}`},
		{"gemini", `{"contents":[{"role":"user","parts":[{"text":"torana> status"}]},{"role":"model","parts":[{"text":"local"}]},{"role":"user","parts":[{"text":"next"}]}]}`, `{"contents":[{"role":"model","parts":[{"text":"local"}]},{"role":"user","parts":[{"text":"next"}]}]}`},
		{"gemini-codeassist", `{"request":{"contents":[{"role":"user","parts":[{"text":"torana> status"}]},{"role":"model","parts":[{"text":"local"}]},{"role":"user","parts":[{"text":"next"}]}]}}`, `{"request":{"contents":[{"role":"model","parts":[{"text":"local"}]},{"role":"user","parts":[{"text":"next"}]}]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.shape, func(t *testing.T) {
			got, commands, changed, err := stripDirectiveText([]byte(tc.body), tc.shape, nil)
			if err != nil || !changed || len(commands) != 0 || string(got) != tc.want {
				t.Fatalf("strip=%s, commands=%+v, changed=%v, err=%v; want=%s", got, commands, changed, err, tc.want)
			}
		})
	}
}

func TestDirectiveOnlyCurrentTurnRemainsParseable(t *testing.T) {
	cases := []struct{ shape, body string }{
		{"openai", `{"model":"m","messages":[{"role":"user","content":"torana> status"}]}`},
		{"anthropic", `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"torana> status"}]}`},
		{"gemini", `{"contents":[{"role":"user","parts":[{"text":"torana> status"}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.shape, func(t *testing.T) {
			body, commands, _, err := stripDirectiveText([]byte(tc.body), tc.shape, nil)
			if err != nil || len(commands) != 1 {
				t.Fatalf("strip commands=%+v err=%v", commands, err)
			}
			if _, err := format.Lookup(tc.shape).Request.Unmarshal(body); err != nil {
				t.Fatalf("current local turn became unparseable: %v", err)
			}
		})
	}
}
