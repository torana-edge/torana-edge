package format_test

import (
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
	_ "github.com/torana-edge/torana-edge/internal/format/anthropic"
	_ "github.com/torana-edge/torana-edge/internal/format/bedrock"
	_ "github.com/torana-edge/torana-edge/internal/format/gemini"
	_ "github.com/torana-edge/torana-edge/internal/format/openai"
)

// TestProviderCacheCapabilityInventory is the executable counterpart of the
// provider matrix in docs/PROMPT_CACHING.md. An explicit inference-request
// breakpoint is the prerequisite for cache-tier selection and warming;
// preserving an automatic-cache hint or an external cache-resource reference
// must never be mistaken for that authority.
func TestProviderCacheCapabilityInventory(t *testing.T) {
	type cacheKind uint8
	const (
		cacheKindExplicitBreakpoint cacheKind = iota + 1
		cacheKindAutomaticHints
		cacheKindExternalResource
	)
	rows := []struct {
		name     string
		format   string
		request  string
		wireFact string
		kind     cacheKind
	}{
		{
			name: "anthropic explicit breakpoint", format: "anthropic",
			request:  `{"model":"claude","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`,
			wireFact: `"cache_control":{"type":"ephemeral"}`,
			kind:     cacheKindExplicitBreakpoint,
		},
		{
			name: "bedrock explicit breakpoint", format: "bedrock",
			request:  `{"modelId":"claude","messages":[{"role":"user","content":[{"text":"hi"},{"cachePoint":{"type":"default"}}]}]}`,
			wireFact: `"cachePoint":{"type":"default"}`,
			kind:     cacheKindExplicitBreakpoint,
		},
		{
			name: "openai automatic cache hints", format: "openai",
			request:  `{"model":"gpt","messages":[{"role":"user","content":"hi"}],"prompt_cache_key":"session","prompt_cache_retention":"24h"}`,
			wireFact: `"prompt_cache_retention":"24h"`,
			kind:     cacheKindAutomaticHints,
		},
		{
			name: "gemini external cache reference", format: "gemini",
			request:  `{"cachedContent":"cachedContents/abc","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			wireFact: `"cachedContent":"cachedContents/abc"`,
			kind:     cacheKindExternalResource,
		},
		{
			name: "code assist external cache reference", format: "gemini-codeassist",
			request:  `{"model":"gemini","request":{"cachedContent":"cachedContents/abc","contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`,
			wireFact: `"cachedContent":"cachedContents/abc"`,
			kind:     cacheKindExternalResource,
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := format.Lookup(row.format)
			if f == nil {
				t.Fatalf("format %q is not registered", row.format)
			}
			req, err := f.Request.Unmarshal([]byte(row.request))
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			wantBreakpoint := row.kind == cacheKindExplicitBreakpoint
			if got := hasExplicitBreakpoint(req); got != wantBreakpoint {
				t.Fatalf("explicit breakpoint = %v, want %v for cache kind %d", got, wantBreakpoint, row.kind)
			}

			out, err := f.Request.Marshal(req)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if !strings.Contains(string(out), row.wireFact) {
				t.Fatalf("final wire lost cache fact %s: %s", row.wireFact, out)
			}
		})
	}
}

func hasExplicitBreakpoint(req *engine.ChatRequest) bool {
	for _, tool := range req.Tools {
		if !tool.CacheControl.IsAbsent() {
			return true
		}
	}
	for _, msg := range req.Messages {
		for _, block := range msg.Blocks {
			if block.CacheBreakpoint != nil {
				return true
			}
			if block.ToolResult == nil {
				continue
			}
			for _, content := range block.ToolResult.Content {
				if content.CacheBreakpoint != nil {
					return true
				}
			}
		}
	}
	return false
}
