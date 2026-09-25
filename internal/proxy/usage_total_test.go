package proxy

import (
	"math"
	"testing"
)

func TestPluginVisibleInputTokensIncludeAnthropicCache(t *testing.T) {
	for _, tc := range []struct {
		format string
		want   int
	}{
		{"anthropic", 60000},
		{"openai", 60000},
		{"gemini", 60000},
	} {
		t.Run(tc.format, func(t *testing.T) {
			state := &reqState{
				UsageReported: true, UsageFormat: tc.format,
				UsageIn: 60000, UsageCacheRead: 50000, UsageCacheWrite: 5000,
			}
			if tc.format == "anthropic" {
				state.UsageIn = 5000 // Anthropic reports uncached input separately.
			}
			response := state.chatResponse("model", "", nil, "")
			if response.Usage == nil || response.Usage.InputTokens != tc.want {
				t.Fatalf("plugin-visible input=%+v, want %d", response.Usage, tc.want)
			}
			metaUsage := state.responseMeta()["usage"].(map[string]any)
			if metaUsage["input_tokens"] != tc.want {
				t.Fatalf("metadata input=%v, want %d", metaUsage["input_tokens"], tc.want)
			}
			if state.UsageIn != map[string]int{"anthropic": 5000, "openai": 60000, "gemini": 60000}[tc.format] {
				t.Fatal("native provider usage was mutated")
			}
		})
	}
}

func TestInvalidTotalUsageIsNotExposedToPlugins(t *testing.T) {
	state := &reqState{UsageReported: true, UsageFormat: "anthropic", UsageIn: math.MaxInt32, UsageCacheRead: 1}
	if response := state.chatResponse("model", "", nil, ""); response.Usage != nil {
		t.Fatalf("overflowing usage reached plugin: %+v", response.Usage)
	}
}
