package plugin

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

// requireWASM skips the test locally when the plugin binary hasn't been
// built, but fails hard in CI (TORANA_E2E=1) so missing binaries can never
// silently disable coverage again.
func requireWASM(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		if os.Getenv("TORANA_E2E") != "" {
			t.Fatalf("%s missing — run 'make plugins testdata' (err: %v)", path, err)
		}
		t.Skipf("%s not built — run 'make plugins testdata'", path)
	}
}

func newTestPipeline(t *testing.T, dir string, order []string) *PluginPipeline {
	t.Helper()
	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { rt.Close() })
	pp, err := NewPipeline(rt, PluginConfig{Dir: dir, Order: order})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if pp.Len() != len(order) {
		t.Fatalf("loaded %d plugins, want %d", pp.Len(), len(order))
	}
	return pp
}

func toolStart(index int, id, name string) engine.StreamEvent {
	return engine.StreamEvent{ToolCallStart: &engine.ToolCallStart{Index: index, ID: id, Name: name}}
}

func toolDelta(index int, frag string) engine.StreamEvent {
	return engine.StreamEvent{ToolCallDelta: &engine.ToolCallDelta{Index: index, ArgumentsDelta: frag}}
}

func toolEnd(index int) engine.StreamEvent {
	return engine.StreamEvent{ToolCallEnd: &engine.ToolCallEnd{Index: index}}
}

func run(t *testing.T, pp *PluginPipeline, ev engine.StreamEvent) []engine.StreamEvent {
	return runAs(t, pp, 1, ev)
}

func runAs(t *testing.T, pp *PluginPipeline, reqID uint64, ev engine.StreamEvent) []engine.StreamEvent {
	t.Helper()
	out, err := pp.RunOnStreamChunk(context.Background(), reqID, &ev)
	if err != nil {
		t.Fatalf("RunOnStreamChunk: %v", err)
	}
	return out
}

// TestStreamPassthrough: events a plugin doesn't handle flow through 1:1.
func TestStreamPassthrough(t *testing.T) {
	requireWASM(t, "../../plugins/schema_translator/plugin.wasm")
	pp := newTestPipeline(t, "../../plugins", []string{"schema_translator"})

	text := "hello"
	out := run(t, pp, engine.StreamEvent{TextDelta: &text})
	if len(out) != 1 || out[0].TextDelta == nil || *out[0].TextDelta != "hello" {
		t.Fatalf("expected passthrough text delta, got %+v", out)
	}
}

// TestStreamSuppressAndFanOut: argument fragments are suppressed, and
// ToolCallEnd fans out into [complete reversed delta, end].
func TestStreamSuppressAndFanOut(t *testing.T) {
	requireWASM(t, "../../plugins/schema_translator/plugin.wasm")
	pp := newTestPipeline(t, "../../plugins", []string{"schema_translator"})

	out := run(t, pp, toolStart(0, "call_1", "write"))
	if len(out) != 1 || out[0].ToolCallStart == nil {
		t.Fatalf("expected ToolCallStart passthrough, got %+v", out)
	}

	// Fragmented KV-array args: {"env":[{"key":"A","value":"1"}]}
	if out := run(t, pp, toolDelta(0, `{"env":[{"key":"A",`)); len(out) != 0 {
		t.Fatalf("expected fragment suppressed, got %+v", out)
	}
	if out := run(t, pp, toolDelta(0, `"value":"1"}]}`)); len(out) != 0 {
		t.Fatalf("expected fragment suppressed, got %+v", out)
	}

	out = run(t, pp, toolEnd(0))
	if len(out) != 2 {
		t.Fatalf("expected [delta, end] fan-out, got %d events: %+v", len(out), out)
	}
	if out[0].ToolCallDelta == nil || out[1].ToolCallEnd == nil {
		t.Fatalf("expected [delta, end], got %+v", out)
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(out[0].ToolCallDelta.ArgumentsDelta), &args); err != nil {
		t.Fatalf("emitted args not valid JSON: %v (%q)", err, out[0].ToolCallDelta.ArgumentsDelta)
	}
	env, ok := args["env"].(map[string]any)
	if !ok {
		t.Fatalf("expected env reversed to object, got %T: %v", args["env"], args["env"])
	}
	if env["A"] != "1" {
		t.Fatalf("expected env.A=1, got %v", env)
	}
}

// TestStreamReplace: a plugin can replace an event in place.
func TestStreamReplace(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-stream-mutator/plugin.wasm")
	pp := newTestPipeline(t, "../../examples/plugins", []string{"test-stream-mutator"})

	text := "the secret plan"
	out := run(t, pp, engine.StreamEvent{TextDelta: &text})
	if len(out) != 1 || out[0].TextDelta == nil {
		t.Fatalf("expected single replaced text delta, got %+v", out)
	}
	if *out[0].TextDelta != "the [REDACTED] plan" {
		t.Fatalf("expected redaction, got %q", *out[0].TextDelta)
	}
}

// TestStreamRequestIsolation: two interleaved requests buffering fragments
// at the same tool index must not contaminate each other — the exact
// corruption the pre-fix global meta store guaranteed under concurrency.
func TestStreamRequestIsolation(t *testing.T) {
	requireWASM(t, "../../plugins/schema_translator/plugin.wasm")
	pp := newTestPipeline(t, "../../plugins", []string{"schema_translator"})

	// Both requests use index 0 and stream interleaved fragments.
	runAs(t, pp, 1, toolStart(0, "call_r1", "write"))
	runAs(t, pp, 2, toolStart(0, "call_r2", "write"))
	runAs(t, pp, 1, toolDelta(0, `{"env":[{"key":"A",`))
	runAs(t, pp, 2, toolDelta(0, `{"env":[{"key":"B",`))
	runAs(t, pp, 1, toolDelta(0, `"value":"1"}]}`))
	runAs(t, pp, 2, toolDelta(0, `"value":"2"}]}`))

	for reqID, wantKey := range map[uint64]string{1: "A", 2: "B"} {
		out := runAs(t, pp, reqID, toolEnd(0))
		if len(out) != 2 || out[0].ToolCallDelta == nil {
			t.Fatalf("req %d: expected [delta, end], got %+v", reqID, out)
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(out[0].ToolCallDelta.ArgumentsDelta), &args); err != nil {
			t.Fatalf("req %d: invalid args %q: %v", reqID, out[0].ToolCallDelta.ArgumentsDelta, err)
		}
		env, _ := args["env"].(map[string]any)
		if len(env) != 1 || env[wantKey] == nil {
			t.Fatalf("req %d: expected only env.%s, got %v", reqID, wantKey, args)
		}
	}
}

// TestRegistryPathReversal proves reversal uses the per-request mutation
// registry recorded at RunBeforeRequest — not just the KV-shape heuristic.
// Only the registered path (env) is reversed; an unregistered KV-lookalike
// array (pairs) must survive intact. The heuristic would reverse both.
func TestRegistryPathReversal(t *testing.T) {
	requireWASM(t, "../../plugins/schema_translator/plugin.wasm")
	pp := newTestPipeline(t, "../../plugins", []string{"schema_translator"})

	const reqID = 42
	ctx := context.Background()

	// Request side: tool "write" has an open map at env → recorded as a
	// KV-array mutation in the registry under this request ID.
	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{{
			Name: "write",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"env": map[string]any{
						"type":                 "object",
						"additionalProperties": map[string]any{"type": "string"},
					},
					"pairs": map[string]any{"type": "array"},
				},
			},
		}},
	}
	if _, err := pp.RunBeforeRequest(ctx, reqID, chat); err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}

	// Response side, same request: env is a KV array (registered mutation),
	// pairs is a KV-shaped array the model produced on its own.
	runAs(t, pp, reqID, toolStart(0, "call_reg", "write"))
	runAs(t, pp, reqID, toolDelta(0, `{"env":[{"key":"A","value":"1"}],`))
	runAs(t, pp, reqID, toolDelta(0, `"pairs":[{"key":"k","value":"v"}]}`))

	out := runAs(t, pp, reqID, toolEnd(0))
	if len(out) != 2 || out[0].ToolCallDelta == nil {
		t.Fatalf("expected [delta, end], got %+v", out)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(out[0].ToolCallDelta.ArgumentsDelta), &args); err != nil {
		t.Fatalf("invalid args %q: %v", out[0].ToolCallDelta.ArgumentsDelta, err)
	}
	env, ok := args["env"].(map[string]any)
	if !ok || env["A"] != "1" {
		t.Fatalf("registered path env not reversed: %v", args)
	}
	if _, isArray := args["pairs"].([]any); !isArray {
		t.Fatalf("unregistered pairs was reversed — heuristic ran instead of registry: %v", args)
	}
}

// TestStreamChaining: schema_translator fans out at ToolCallEnd; compactor
// consumes that fan-out, extracts the intent, and strips the "i" field.
func TestStreamChaining(t *testing.T) {
	requireWASM(t, "../../plugins/schema_translator/plugin.wasm")
	requireWASM(t, "../../plugins/compactor/plugin.wasm")
	pp := newTestPipeline(t, "../../plugins", []string{"schema_translator", "compactor"})

	if out := run(t, pp, toolStart(0, "call_2", "write")); len(out) != 1 {
		t.Fatalf("expected start passthrough, got %+v", out)
	}
	// Args carry an injected "i" intent plus a KV-array map.
	if out := run(t, pp, toolDelta(0, `{"i":"find the bug",`)); len(out) != 0 {
		t.Fatalf("expected fragment suppressed, got %+v", out)
	}
	if out := run(t, pp, toolDelta(0, `"env":[{"key":"A","value":"1"}]}`)); len(out) != 0 {
		t.Fatalf("expected fragment suppressed, got %+v", out)
	}

	out := run(t, pp, toolEnd(0))
	if len(out) != 2 || out[0].ToolCallDelta == nil || out[1].ToolCallEnd == nil {
		t.Fatalf("expected [delta, end], got %+v", out)
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(out[0].ToolCallDelta.ArgumentsDelta), &args); err != nil {
		t.Fatalf("emitted args not valid JSON: %v (%q)", err, out[0].ToolCallDelta.ArgumentsDelta)
	}
	if _, hasI := args["i"]; hasI {
		t.Fatalf(`expected "i" stripped by compactor, got %v`, args)
	}
	env, ok := args["env"].(map[string]any)
	if !ok || env["A"] != "1" {
		t.Fatalf("expected env reversed by schema_translator, got %v", args)
	}
}

// TestFewShotPlacementNeverSplitsToolPairs: the compactor's few-shot triplet
// must land after the leading system messages — inserting it before the last
// message split assistant tool_calls from their tool results, which strict
// providers (DeepSeek) reject with a 400. Caught live during dogfooding.
func TestFewShotPlacementNeverSplitsToolPairs(t *testing.T) {
	requireWASM(t, "../../plugins/compactor/plugin.wasm")
	pp := newTestPipeline(t, "../../plugins", []string{"compactor"})

	chat := &engine.ChatRequest{
		Messages: []engine.Message{
			{Role: engine.RoleSystem, Content: "sys"},
			{Role: engine.RoleUser, Content: "do the thing"},
			{Role: engine.RoleAssistant, ToolCalls: []engine.ToolCall{{ID: "call_real_1", Name: "read", Arguments: map[string]any{"path": "x"}}}},
			{Role: engine.RoleTool, ToolCallID: "call_real_1", Content: "result"},
		},
		Tools: []engine.ToolDef{{Name: "read", Parameters: map[string]any{
			"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}},
		}}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 77, chat)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}

	// Every assistant message carrying tool_calls must be immediately
	// followed by tool messages answering each of its call IDs.
	for i, m := range out.Messages {
		if m.Role != engine.RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}
		want := map[string]bool{}
		for _, tc := range m.ToolCalls {
			want[tc.ID] = true
		}
		for j := i + 1; j < len(out.Messages) && len(want) > 0; j++ {
			if out.Messages[j].Role != engine.RoleTool {
				t.Fatalf("assistant tool_calls at %d followed by %q at %d (unanswered: %v)\nsequence: %v",
					i, out.Messages[j].Role, j, want, roles(out.Messages))
			}
			delete(want, out.Messages[j].ToolCallID)
		}
		if len(want) > 0 {
			t.Fatalf("assistant tool_calls at %d never answered: %v", i, want)
		}
	}

	// And the few-shot must sit right after the system message.
	if out.Messages[0].Role != engine.RoleSystem || out.Messages[1].Role != engine.RoleUser ||
		len(out.Messages) < 4 || len(out.Messages[2].ToolCalls) == 0 ||
		out.Messages[2].ToolCalls[0].ID != "call_mock_fewshot_1" {
		t.Fatalf("few-shot not placed after system: %v", roles(out.Messages))
	}
}

func roles(msgs []engine.Message) []string {
	var out []string
	for _, m := range msgs {
		r := string(m.Role)
		if len(m.ToolCalls) > 0 {
			r += "(tool_calls)"
		}
		out = append(out, r)
	}
	return out
}
