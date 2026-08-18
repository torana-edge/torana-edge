package openai

import "testing"

// detectVariant used to scan for substrings anywhere in the body, so it could
// not tell a top-level key from the same characters inside a message. Torana's
// traffic is coding agents, which routinely paste request bodies and JSON into
// prompts — so "messages" and "object":"response" appearing inside content is
// ordinary, not exotic.

func TestDetectVariant(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want variant
	}{
		"chat by messages":                {`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`, variantChat},
		"responses by input":              {`{"model":"gpt-4","input":"hi"}`, variantResponses},
		"responses by object":             {`{"object":"response","output":[]}`, variantResponses},
		"responses object spaced":         {`{"object": "response","output":[]}`, variantResponses},
		"responses input null is present": {`{"model":"gpt-4","input":null}`, variantResponses},
		"both deciding members present":   {`{"input":null,"messages":null}`, variantChat},

		// The cases the substring scan got wrong. Inside a JSON string these
		// keys are escaped, so the scan survived quoted prose — what defeats it
		// is the same word appearing as a REAL key somewhere nested, which
		// needs no escaping at all.
		"responses with a tool that takes a messages parameter": {
			`{"model":"gpt-4","input":"hi","tools":[{"type":"function","name":"send",` +
				`"parameters":{"type":"object","properties":{"messages":{"type":"array"}}}}]}`,
			variantResponses,
		},
		"responses with metadata naming messages": {
			`{"model":"gpt-4","input":"hi","metadata":{"messages":"3"}}`,
			variantResponses,
		},
		"chat with a tool whose schema embeds object:response": {
			`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],` +
				`"tools":[{"type":"function","function":{"name":"f","parameters":` +
				`{"type":"object","properties":{"object":{"const":"response"}}}}}]}`,
			variantChat,
		},
		"chat with a tool that takes an input parameter": {
			`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],` +
				`"tools":[{"type":"function","function":{"name":"f","parameters":` +
				`{"type":"object","properties":{"input":{"type":"string"}}}}}]}`,
			variantChat,
		},

		// Malformed bodies fall back to Chat, whose parser reports a more
		// useful error than a guess here would.
		"not an object":  {`[1,2,3]`, variantChat},
		"truncated json": {`{"model":"gpt-4","inp`, variantChat},
		"empty":          {``, variantChat},
	} {
		t.Run(name, func(t *testing.T) {
			if got := detectVariant([]byte(tc.body)); got != tc.want {
				t.Errorf("detectVariant = %v, want %v", got, tc.want)
			}
		})
	}
}
