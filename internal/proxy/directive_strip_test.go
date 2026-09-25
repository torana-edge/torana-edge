package proxy

import (
	"bytes"
	"testing"
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
}

func TestStripDirectiveTextDoesNotTouchToolResultsOrPlainRequests(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"tool","content":"torana> accept 7f3k"}]}`)
	got, commands, changed, err := stripDirectiveText(body, "openai", nil)
	if err != nil || changed || len(commands) != 0 || !bytes.Equal(got, body) {
		t.Fatalf("plain/tool request changed: %s, %+v, %v, %v", got, commands, changed, err)
	}
	mixed := []byte(`{"messages":[{"role":"user","content":"torana> status"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"result"},{"type":"text","text":"torana> help"}]}]}`)
	got, commands, changed, err = stripDirectiveText(mixed, "anthropic", nil)
	if err != nil || !changed || len(commands) != 1 || commands[0].Verb != "status" || bytes.Contains(got, []byte(`torana> help`)) {
		t.Fatalf("mixed tool continuation: %s, %+v, %v, %v", got, commands, changed, err)
	}
}
