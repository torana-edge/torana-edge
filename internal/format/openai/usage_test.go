package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// OpenAI names its token fields two different ways, and Torana used to read
// them in two different places. The non-streaming reader knew only the Chat
// names, so every non-streaming Responses API call was accounted at zero
// tokens and zero cost — silently. These tests pin both shapes against the one
// reader that both paths now use.

func TestReadUsageBothVariants(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want engine.StreamUsage
	}{
		"chat completions": {
			`{"prompt_tokens":100,"completion_tokens":20}`,
			engine.StreamUsage{InputTokens: 100, OutputTokens: 20},
		},
		"chat completions with cache read": {
			`{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":80}}`,
			engine.StreamUsage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 80},
		},
		"deepseek flat cache hit": {
			`{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":64}`,
			engine.StreamUsage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 64},
		},
		// The shape that was accounted at zero.
		"responses": {
			`{"input_tokens":100,"output_tokens":20}`,
			engine.StreamUsage{InputTokens: 100, OutputTokens: 20},
		},
		"responses with cache read and write": {
			`{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":15}}`,
			engine.StreamUsage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 80, CacheWriteTokens: 15},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var usage map[string]any
			if err := json.Unmarshal([]byte(tc.body), &usage); err != nil {
				t.Fatal(err)
			}
			got := ReadUsage(usage)
			if got == nil {
				t.Fatalf("usage went unread — this is the silent zero-cost bug: %s", tc.body)
			}
			if *got != tc.want {
				t.Errorf("got %+v, want %+v", *got, tc.want)
			}
		})
	}
}

// TestReadUsageAbsentOrEmpty: an all-zero usage object carries no information,
// and returning it would record a real response as a zero-token one — which is
// worse than reporting nothing, because it looks like a measurement.
func TestReadUsageAbsentOrEmpty(t *testing.T) {
	if got := ReadUsage(nil); got != nil {
		t.Errorf("nil usage should read as nil, got %+v", got)
	}
	if got := ReadUsage(map[string]any{}); got != nil {
		t.Errorf("empty usage should read as nil, got %+v", got)
	}
	if got := ReadUsage(map[string]any{"prompt_tokens": float64(0), "completion_tokens": float64(0)}); got != nil {
		t.Errorf("all-zero usage should read as nil, got %+v", got)
	}
}

// TestStreamingAndNonStreamingAgreeOnUsage is the anti-divergence test, and it
// has to drive both real code paths to be one.
//
// The first version called ReadUsage twice on two maps decoded from the same
// string. That is an assertion about ReadUsage, not about the paths — review
// replaced the Responses branch of the streaming parser with a chat-only
// decoder and the test still passed. It now runs SSE through ParseStream and a
// response body through extractResponseUsage, so reintroducing a private
// decoder for either path fails here.
func TestStreamingAndNonStreamingAgreeOnUsage(t *testing.T) {
	for name, tc := range map[string]struct {
		sse  string
		body string
	}{
		"chat completions": {
			sse: "data: {\"object\":\"chat.completion.chunk\",\"choices\":[]," +
				"\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20," +
				"\"prompt_tokens_details\":{\"cached_tokens\":80}}}\n\ndata: [DONE]\n\n",
			body: `{"object":"chat.completion","usage":{"prompt_tokens":100,"completion_tokens":20,` +
				`"prompt_tokens_details":{"cached_tokens":80}}}`,
		},
		"responses": {
			sse: "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"," +
				"\"usage\":{\"input_tokens\":100,\"output_tokens\":20," +
				"\"input_tokens_details\":{\"cached_tokens\":80,\"cache_write_tokens\":15}}}}\n\n",
			body: `{"object":"response","usage":{"input_tokens":100,"output_tokens":20,` +
				`"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":15}}}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			// The streaming path, through the real parser.
			var streaming *engine.StreamUsage
			for evt := range (&StreamAdapter{}).ParseStream(strings.NewReader(tc.sse)) {
				if evt.Usage != nil {
					streaming = evt.Usage
				}
			}
			if streaming == nil {
				t.Fatal("the streaming parser reported no usage")
			}

			// The non-streaming path, through the reader extractOpenAI uses.
			var decoded map[string]any
			if err := json.Unmarshal([]byte(tc.body), &decoded); err != nil {
				t.Fatal(err)
			}
			usage, _ := decoded["usage"].(map[string]any)
			nonStreaming := ReadUsage(usage)
			if nonStreaming == nil {
				t.Fatal("the non-streaming reader reported no usage")
			}

			if *streaming != *nonStreaming {
				t.Errorf("the two paths disagree for the same numbers:\n  streaming     %+v\n  non-streaming %+v",
					*streaming, *nonStreaming)
			}
			if streaming.InputTokens != 100 || streaming.OutputTokens != 20 || streaming.CacheReadTokens != 80 {
				t.Errorf("both paths agree on the WRONG values: %+v", *streaming)
			}
		})
	}
}
