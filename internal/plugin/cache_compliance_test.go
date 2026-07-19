package plugin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

// Prompt-cache compliance harness.
//
// Contract (docs/PLUGIN_IMPLEMENTATION_GUIDE.md "Prompt-Cache Compliance"):
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
			{Role: engine.RoleSystem, Content: "You are a coding agent.",
				CacheControl: map[string]any{"type": "ephemeral"}},
			{Role: engine.RoleUser, Content: "find the bug in server.go"},
			{Role: engine.RoleAssistant, ToolCalls: []engine.ToolCall{{
				ID: "call_1", Name: "read",
				Arguments: map[string]any{"path": "server.go"}, // no "i": forces heuristic fill
			}}},
			{Role: engine.RoleTool, ToolCallID: "call_1", ToolName: "read", Content: bigResult},
			{Role: engine.RoleUser, Content: "now fix it",
				CacheControl: map[string]any{"type": "ephemeral"}},
		},
		Tools: []engine.ToolDef{
			{Name: "read", Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
			}},
			{Name: "write", Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}},
			}, CacheControl: map[string]any{"type": "ephemeral"}},
		},
	}
}

// stableBytes renders the request for comparison. ToranaMeta is excluded: it
// is proxy-internal, never serialized to the wire, and legitimately carries
// per-request state (mutation registries).
func stableBytes(t *testing.T, chat *engine.ChatRequest) []byte {
	t.Helper()
	clone := *chat
	clone.ToranaMeta = nil
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
	for _, name := range []string{"schema_translator", "intent", "keyword_compactor", "compactor", "pii", "otel"} {
		t.Run(name, func(t *testing.T) {
			requireWASM(t, "../../plugins/"+name+"/plugin.wasm")

			ctx := context.Background()
			runtime := wasm.NewRuntime(ctx)
			defer runtime.Close()

			pipeline, err := NewPipeline(runtime, PluginConfig{
				Dir:   "../../plugins",
				Order: []string{name},
			})
			if err != nil {
				t.Fatalf("NewPipeline: %v", err)
			}
			if pipeline.Len() != 1 {
				t.Fatalf("%s plugin not loaded (loaded=%d)", name, pipeline.Len())
			}

			run := func(reqID uint64) []byte {
				out, err := pipeline.RunBeforeRequest(ctx, reqID, cacheComplianceRequest())
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
	requireWASM(t, "../../plugins/intent/plugin.wasm")

	ctx := context.Background()
	runtime := wasm.NewRuntime(ctx)
	defer runtime.Close()

	pipeline, err := NewPipeline(runtime, PluginConfig{
		Dir:   "../../plugins",
		Order: []string{"intent"},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	out, err := pipeline.RunBeforeRequest(ctx, 1, cacheComplianceRequest())
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}

	// The intent plugin mutates tools AND messages, so the returned request
	// went through a full pb round-trip.
	if out.Messages[0].Role != engine.RoleSystem || out.Messages[0].CacheControl == nil {
		t.Errorf("system message cache_control stripped by plugin round-trip: %+v", out.Messages[0])
	}
	last := out.Messages[len(out.Messages)-1]
	if last.CacheControl == nil {
		t.Errorf("user message cache_control stripped by plugin round-trip: %+v", last)
	}
	var marked bool
	for _, td := range out.Tools {
		if td.Name == "write" && td.CacheControl != nil {
			marked = true
		}
	}
	if !marked {
		t.Errorf("tool def cache_control stripped by plugin round-trip: %+v", out.Tools)
	}
}
