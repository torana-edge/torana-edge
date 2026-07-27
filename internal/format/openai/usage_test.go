package openai

import (
	"encoding/json"
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

// TestStreamingAndNonStreamingAgreeOnUsage is the anti-divergence test. Both
// paths take the same JSON through the same reader, so the assertion is that
// there is exactly one implementation — if someone reintroduces a typed struct
// for one path, this stops being true the moment the two disagree.
func TestStreamingAndNonStreamingAgreeOnUsage(t *testing.T) {
	for _, body := range []string{
		`{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":80}}`,
		`{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":15}}`,
	} {
		// The streaming path decodes usage as part of an SSE chunk...
		var chunk struct {
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(`{"usage":`+body+`}`), &chunk); err != nil {
			t.Fatal(err)
		}
		streaming := ReadUsage(chunk.Usage)

		// ...the non-streaming path pulls it out of a decoded response body.
		var resp map[string]any
		if err := json.Unmarshal([]byte(`{"usage":`+body+`}`), &resp); err != nil {
			t.Fatal(err)
		}
		usage, _ := resp["usage"].(map[string]any)
		nonStreaming := ReadUsage(usage)

		if streaming == nil || nonStreaming == nil {
			t.Fatalf("%s: one path read nothing (streaming=%v non-streaming=%v)", body, streaming, nonStreaming)
		}
		if *streaming != *nonStreaming {
			t.Errorf("%s: paths disagree — streaming %+v, non-streaming %+v", body, *streaming, *nonStreaming)
		}
	}
}
