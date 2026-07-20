package proxy

import (
	"encoding/json"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// TestExtractResponseCacheUsage: each provider's non-streaming JSON usage
// object reports cached tokens under a different key — all four must land on
// StreamUsage.CacheRead/WriteTokens (they were dropped before the fix).
func TestExtractResponseCacheUsage(t *testing.T) {
	cases := []struct {
		format string
		body   string
		read   int
		write  int
	}{
		{
			format: "anthropic",
			body:   `{"model":"claude-sonnet-4","content":[],"usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":9000,"cache_creation_input_tokens":1200}}`,
			read:   9000, write: 1200,
		},
		{
			format: "openai",
			body:   `{"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":2000,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":1792}}}`,
			read:   1792, write: 0,
		},
		{
			format: "openai",
			body:   `{"model":"deepseek-v4-pro","choices":[],"usage":{"prompt_tokens":2000,"completion_tokens":10,"prompt_cache_hit_tokens":1536,"prompt_cache_miss_tokens":464}}`,
			read:   1536, write: 0,
		},
		{
			// Standard OpenAI details win if a compatible provider emits both.
			format: "openai",
			body:   `{"model":"compatible","choices":[],"usage":{"prompt_tokens":2000,"completion_tokens":10,"prompt_cache_hit_tokens":1536,"prompt_tokens_details":{"cached_tokens":1024}}}`,
			read:   1024, write: 0,
		},
		{
			format: "bedrock",
			body:   `{"output":{"message":{"content":[]}},"usage":{"inputTokens":10,"outputTokens":4,"cacheReadInputTokens":8000,"cacheWriteInputTokens":500}}`,
			read:   8000, write: 500,
		},
		{
			format: "gemini",
			body:   `{"candidates":[],"usageMetadata":{"promptTokenCount":32179,"candidatesTokenCount":196,"cachedContentTokenCount":24430}}`,
			read:   24430, write: 0,
		},
		{
			// Code Assist wrapper: same fields nested under "response".
			format: "gemini-codeassist",
			body:   `{"response":{"candidates":[],"usageMetadata":{"promptTokenCount":32179,"candidatesTokenCount":196,"cachedContentTokenCount":24430}}}`,
			read:   24430, write: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
				t.Fatal(err)
			}
			u := extractResponse(tc.format, body).usage
			if u == nil {
				t.Fatal("no usage extracted")
			}
			if u.CacheReadTokens != tc.read || u.CacheWriteTokens != tc.write {
				t.Errorf("cache read/write = %d/%d, want %d/%d", u.CacheReadTokens, u.CacheWriteTokens, tc.read, tc.write)
			}
		})
	}
}

// TestMergeUsagePreservesAcrossFrames: Anthropic splits usage across
// message_start (input + cache) and message_delta (output); merging must not
// let the later frame zero the earlier counts.
func TestMergeUsagePreservesAcrossFrames(t *testing.T) {
	rs := &reqState{}
	rs.mergeUsage(&engine.StreamUsage{InputTokens: 42, CacheReadTokens: 9000, CacheWriteTokens: 1200})
	rs.mergeUsage(&engine.StreamUsage{OutputTokens: 5})
	if rs.UsageIn != 42 || rs.UsageOut != 5 || rs.UsageCacheRead != 9000 || rs.UsageCacheWrite != 1200 {
		t.Errorf("merged usage = in=%d out=%d read=%d write=%d, want 42/5/9000/1200",
			rs.UsageIn, rs.UsageOut, rs.UsageCacheRead, rs.UsageCacheWrite)
	}
}
