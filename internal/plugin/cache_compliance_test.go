package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/wasm"

	_ "github.com/torana-edge/torana-edge/internal/format/anthropic"
	_ "github.com/torana-edge/torana-edge/internal/format/bedrock"
	_ "github.com/torana-edge/torana-edge/internal/format/gemini"
	_ "github.com/torana-edge/torana-edge/internal/format/openai"
)

// Prompt-cache compliance harness.
//
// Contract (torana-plugin-sdk docs/PLUGIN_SEMANTICS.md "Prompt-Cache Compliance"):
// a plugin's transformation of the cacheable prefix (tools, system, history)
// must be a deterministic function of its input, and cache_control markers
// must survive the plugin round-trip. A plugin that injects per-request
// content (wall clock, request IDs, snippets of the latest message) into the
// prefix re-serializes identical history to different bytes each turn, which
// busts the provider prompt cache (OpenAI exact-prefix, Anthropic breakpoint
// hash) and silently multiplies input-token spend.

// cacheComplianceRequest builds a representative agent request: marked system
// prompt, marked tool defs, replayed history with an "i"-less assistant tool
// call (exercises the intent plugin's heuristic fill), a large tool result
// (exercises the compactors), and a marked recent turn.
func cacheComplianceRequest() *engine.ChatRequest {
	bigResult := ""
	for i := 0; i < 200; i++ {
		bigResult += "line of tool output that is long enough to be compaction-eligible\n"
	}
	return &engine.ChatRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []engine.Message{
			{Role: engine.RoleSystem, Blocks: []engine.Block{
				{Text: &engine.TextBlock{Text: "You are a coding agent."}},
				{CacheBreakpoint: &engine.CacheBreakpointBlock{Marker: mustMarker(`{"type":"ephemeral"}`)}},
			}},
			{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "find the bug in server.go"}}}},
			{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{
				ID: "call_1", Name: "read",
				Arguments: mustReqCompliance(`{"path": "server.go"}`), // no "i": forces heuristic fill
			}}}},
			{Role: engine.RoleTool, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{
				ToolCallID: "call_1", ToolName: "read", Content: []engine.ToolResultContentBlock{{Text: bigResult}},
			}}}},
			{Role: engine.RoleUser, Blocks: []engine.Block{
				{Text: &engine.TextBlock{Text: "now fix it"}},
				{CacheBreakpoint: &engine.CacheBreakpointBlock{Marker: mustMarker(`{"type":"ephemeral"}`)}},
			}},
		},
		Tools: []engine.ToolDef{
			{Name: "read", Parameters: mustReqCompliance(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
			{Name: "write", Parameters: mustReqCompliance(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}`),
				CacheControl: mustOptMarker(`{"type":"ephemeral"}`)},
		},
	}
}

// mustReq panics on invalid raw: test fixtures are trusted, and a fixture
// that no longer parses must fail loudly.
func hasCacheBreakpoint(m engine.Message) bool {
	for _, b := range m.Blocks {
		if b.CacheBreakpoint != nil {
			return true
		}
	}
	return false
}

func mustOptMarker(raw string) engine.OptionalJSONObject {
	r, err := engine.ParseOptionalJSONObject([]byte(raw))
	if err != nil {
		panic(err)
	}
	return r
}

func mustMarker(raw string) engine.RequiredJSONObject {
	r, err := engine.ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		panic(err)
	}
	return r
}

func mustReqCompliance(raw string) engine.RequiredJSONObject {
	r, err := engine.ParseRequiredJSONObject([]byte(raw))
	if err != nil {
		panic(err)
	}
	return r
}

// stableBytes renders the request for comparison. ToranaMeta is excluded: it
// is proxy-internal, never serialized to the wire, and legitimately carries
// per-request state (mutation registries).
func stableBytes(t *testing.T, chat *engine.ChatRequest) []byte {
	t.Helper()
	clone := *chat
	clone.ToranaMeta = engine.OptionalJSONObject{}
	b, err := json.Marshal(&clone)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestPluginPrefixDeterminism runs every request-mutating in-repo plugin
// twice over the identical request and asserts byte-identical output — the
// guardrail that keeps plugins from busting turn-over-turn prompt caching.
func TestPluginPrefixDeterminism(t *testing.T) {
	bundles := officialBundlesDir(t)
	for _, name := range []string{"schema_translator", "intent", "keyword_compactor", "compactor", "pii", "otel", "cache_tier_selector", "cache_warmer"} {
		t.Run(name, func(t *testing.T) {
			requireBundle(t, bundles, name)

			ctx := context.Background()
			runtime := wasm.NewRuntime(ctx)
			defer runtime.Close()

			pipeline, err := NewPipeline(runtime, PluginConfig{
				Dir:             bundles,
				Order:           []string{name},
				AllowUnapproved: true,
			})
			if err != nil {
				t.Fatalf("NewPipeline: %v", err)
			}
			if pipeline.Len() != 1 {
				t.Fatalf("%s plugin not loaded (loaded=%d)", name, pipeline.Len())
			}

			run := func(reqID uint64) []byte {
				out, err := pipeline.RunBeforeRequest(ctx, reqID, cacheComplianceRequest(), nil)
				if err != nil {
					t.Fatalf("RunBeforeRequest: %v", err)
				}
				return stableBytes(t, out)
			}

			first := run(1)
			second := run(2)
			if string(first) != string(second) {
				t.Errorf("%s is not deterministic over an identical request — this busts provider prompt caching.\nrun1: %s\nrun2: %s",
					name, first, second)
			}
		})
	}
}

// TestCacheControlSurvivesPluginRoundTrip asserts the structural half of the
// contract: cache_control markers on messages and tool defs survive the
// pb round-trip through a mutating plugin (threaded via pbconv — a plugin
// returning a request must not strip them).
func TestCacheControlSurvivesPluginRoundTrip(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")

	ctx := context.Background()
	runtime := wasm.NewRuntime(ctx)
	defer runtime.Close()

	pipeline, err := NewPipeline(runtime, PluginConfig{
		Dir:             fixturesDir,
		Order:           []string{"test-mutator"},
		AllowUnapproved: true,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	out, err := pipeline.RunBeforeRequest(ctx, 1, cacheComplianceRequest(), nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}

	// The intent plugin mutates tools AND messages, so the returned request
	// went through a full pb round-trip.
	if out.Messages[0].Role != engine.RoleSystem || !hasCacheBreakpoint(out.Messages[0]) {
		t.Errorf("system message cache_control stripped by plugin round-trip: %+v", out.Messages[0])
	}
	last := out.Messages[len(out.Messages)-1]
	if !hasCacheBreakpoint(last) {
		t.Errorf("user message cache_control stripped by plugin round-trip: %+v", last)
	}
	var marked bool
	for _, td := range out.Tools {
		if td.Name == "write" && !td.CacheControl.IsAbsent() {
			marked = true
		}
	}
	if !marked {
		t.Errorf("tool def cache_control stripped by plugin round-trip: %+v", out.Tools)
	}
}

// TestProviderCacheFactsSurviveRealWASMToFinalWire closes the gap between the
// format-only round trips and the canonical plugin compliance test. A real
// mutating WASM guest changes message/tool content, after which each provider
// adapter must still emit its provider-owned cache facts on the final wire.
func TestProviderCacheFactsSurviveRealWASMToFinalWire(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")

	ctx := context.Background()
	runtime := wasm.NewRuntime(ctx)
	defer runtime.Close()
	pipeline, err := NewPipeline(runtime, PluginConfig{
		Dir:             fixturesDir,
		Order:           []string{"test-mutator"},
		AllowUnapproved: true,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	type wireFact struct {
		text  string
		count int
	}

	rows := []struct {
		name      string
		format    string
		request   string
		wireFacts []wireFact
	}{
		{
			name: "anthropic ordered breakpoints", format: "anthropic",
			request:   `{"model":"claude","max_tokens":8,"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}],"tools":[{"name":"read","description":"","input_schema":{},"cache_control":{"type":"ephemeral"}}]}`,
			wireFacts: []wireFact{{`"cache_control"`, 3}, {`"ttl":"1h"`, 1}, {`"content":[{"type":"text","text":"hi [seen by test-mutator]"`, 1}},
		},
		{
			name: "bedrock positional breakpoints", format: "bedrock",
			request:   `{"modelId":"m","system":[{"text":"sys"},{"cachePoint":{"type":"default"}}],"messages":[{"role":"user","content":[{"text":"hi"},{"cachePoint":{"type":"default"}}]}],"toolConfig":{"tools":[{"toolSpec":{"name":"read","description":"","inputSchema":{"json":{}}}},{"cachePoint":{"type":"default"}}]}}`,
			wireFacts: []wireFact{{`"cachePoint":{"type":"default"}`, 3}, {`"text":"hi [seen by test-mutator]"`, 1}},
		},
		{
			name: "openai automatic cache controls", format: "openai",
			request:   `{"model":"gpt","messages":[{"role":"user","content":"hi"}],"prompt_cache_key":"session","prompt_cache_retention":"24h"}`,
			wireFacts: []wireFact{{`"prompt_cache_key":"session"`, 1}, {`"prompt_cache_retention":"24h"`, 1}, {`"content":"hi [seen by test-mutator]"`, 1}},
		},
		{
			name: "openai responses automatic cache controls", format: "openai",
			request:   `{"model":"gpt","input":[{"role":"user","content":"hi"}],"prompt_cache_key":"session","prompt_cache_retention":"24h"}`,
			wireFacts: []wireFact{{`"prompt_cache_key":"session"`, 1}, {`"prompt_cache_retention":"24h"`, 1}, {`"content":"hi [seen by test-mutator]"`, 1}},
		},
		{
			name: "gemini external cache reference", format: "gemini",
			request:   `{"cachedContent":"cachedContents/abc","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			wireFacts: []wireFact{{`"cachedContent":"cachedContents/abc"`, 1}, {`"text":"hi [seen by test-mutator]"`, 1}},
		},
		{
			name: "code assist external cache reference", format: "gemini-codeassist",
			request:   `{"model":"gemini","request":{"cachedContent":"cachedContents/abc","contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`,
			wireFacts: []wireFact{{`"cachedContent":"cachedContents/abc"`, 1}, {`"text":"hi [seen by test-mutator]"`, 1}},
		},
	}

	for i, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := format.Lookup(row.format)
			if f == nil {
				t.Fatalf("format %q is not registered", row.format)
			}
			in, err := f.Request.Unmarshal([]byte(row.request))
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			out, err := pipeline.RunBeforeRequest(ctx, uint64(i+1), in, nil)
			if err != nil {
				t.Fatalf("RunBeforeRequest: %v", err)
			}
			wire, err := f.Request.Marshal(out)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			for _, fact := range row.wireFacts {
				if got := strings.Count(string(wire), fact.text); got != fact.count {
					t.Errorf("final wire occurrence count for %s = %d, want %d: %s", fact.text, got, fact.count, wire)
				}
			}
		})
	}
}
