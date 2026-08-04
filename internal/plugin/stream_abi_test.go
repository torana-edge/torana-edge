package plugin

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/cache"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format/gemini"
	"github.com/torana-edge/torana-edge/internal/format/openai"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

// requireWASM skips the test locally when the plugin binary hasn't been
// built, but fails hard in CI (TORANA_E2E=1) so missing binaries can never
// silently disable coverage again.
// These take testing.TB rather than *testing.T so the benchmarks in
// bench_test.go can build a pipeline through exactly the same path the tests
// do — a benchmark measuring a differently-constructed pipeline would not be
// measuring what ships.
func requireWASM(t testing.TB, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		if os.Getenv("TORANA_E2E") != "" {
			t.Fatalf("%s missing — run 'make testdata' (err: %v)", path, err)
		}
		t.Skipf("%s not built — run 'make testdata'", path)
	}
}

func newTestPipeline(t testing.TB, dir string, order []string) *PluginPipeline {
	t.Helper()
	return newTestPipelineWith(t, dir, order, cache.NewLocalCache(time.Minute), nil)
}

// newTestPipelineWith exposes the plugin cache store to the test (for
// asserting what plugins wrote cross-request) and per-plugin config blobs.
func newTestPipelineWith(t testing.TB, dir string, order []string, store cache.Store, cfg map[string]json.RawMessage) *PluginPipeline {
	t.Helper()
	rt := wasm.NewRuntimeWithCache(context.Background(), store)
	t.Cleanup(func() {
		rt.Close()
		store.Close()
	})
	pp, err := NewPipeline(rt, PluginConfig{Dir: dir, Order: order, Config: cfg, AllowUnapproved: true})
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

// registerEnvMap runs the request side for a tool whose `env` parameter is an
// open map, so schema_translator records the KV-array mutation for reqID. This
// mirrors the real request→response flow: the schema is always translated
// (and the mutation registered) before the response stream is reversed. Tests
// that reverse `env` MUST set this up rather than relying on shape-guessing.
func registerEnvMap(t *testing.T, pp *PluginPipeline, reqID uint64, toolName string) {
	t.Helper()
	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{{
			Name:       toolName,
			Parameters: mustReq(`{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`),
		}},
	}
	if _, err := pp.RunBeforeRequest(context.Background(), reqID, chat, nil); err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
}

// TestStreamPassthrough: events a plugin doesn't handle flow through 1:1.
func TestStreamPassthrough(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-stream-mutator/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-stream-mutator"})

	text := "hello"
	out := run(t, pp, engine.StreamEvent{TextDelta: &text})
	if len(out) != 1 || out[0].TextDelta == nil || *out[0].TextDelta != "hello" {
		t.Fatalf("expected passthrough text delta, got %+v", out)
	}
}

// TestStreamSuppressAndFanOut: argument fragments are suppressed, and
// ToolCallEnd fans out into [complete reversed delta, end].
func TestStreamSuppressAndFanOut(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "schema_translator")
	pp := newTestPipeline(t, bundles, []string{"schema_translator"})

	// Translate the schema first so env's KV-array mutation is registered.
	registerEnvMap(t, pp, 1, "write")

	out := run(t, pp, toolStart(0, "call_1", "write"))
	if len(out) != 0 {
		t.Fatalf("expected the start suppressed (host-side assembly), got %+v", out)
	}

	// Fragmented KV-array args: {"env":[{"key":"A","value":"1"}]}
	if out := run(t, pp, toolDelta(0, `{"env":[{"key":"A",`)); len(out) != 0 {
		t.Fatalf("expected fragment suppressed, got %+v", out)
	}
	if out := run(t, pp, toolDelta(0, `"value":"1"}]}`)); len(out) != 0 {
		t.Fatalf("expected fragment suppressed, got %+v", out)
	}

	out = run(t, pp, toolEnd(0))
	if len(out) != 3 {
		t.Fatalf("expected [start, delta, end] fan-out, got %d events: %+v", len(out), out)
	}
	if out[0].ToolCallStart == nil || out[1].ToolCallDelta == nil || out[2].ToolCallEnd == nil {
		t.Fatalf("expected [start, delta, end], got %+v", out)
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(out[1].ToolCallDelta.ArgumentsDelta), &args); err != nil {
		t.Fatalf("emitted args not valid JSON: %v (%q)", err, out[1].ToolCallDelta.ArgumentsDelta)
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
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "schema_translator")
	pp := newTestPipeline(t, bundles, []string{"schema_translator"})

	// Each request translates its schema first, registering env's mutation.
	registerEnvMap(t, pp, 1, "write")
	registerEnvMap(t, pp, 2, "write")

	// Both requests use index 0 and stream interleaved fragments.
	runAs(t, pp, 1, toolStart(0, "call_r1", "write"))
	runAs(t, pp, 2, toolStart(0, "call_r2", "write"))
	runAs(t, pp, 1, toolDelta(0, `{"env":[{"key":"A",`))
	runAs(t, pp, 2, toolDelta(0, `{"env":[{"key":"B",`))
	runAs(t, pp, 1, toolDelta(0, `"value":"1"}]}`))
	runAs(t, pp, 2, toolDelta(0, `"value":"2"}]}`))

	for reqID, wantKey := range map[uint64]string{1: "A", 2: "B"} {
		out := runAs(t, pp, reqID, toolEnd(0))
		if len(out) != 3 || out[0].ToolCallStart == nil || out[1].ToolCallDelta == nil || out[2].ToolCallEnd == nil {
			t.Fatalf("req %d: expected [start, delta, end], got %+v", reqID, out)
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(out[1].ToolCallDelta.ArgumentsDelta), &args); err != nil {
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
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "schema_translator")
	pp := newTestPipeline(t, bundles, []string{"schema_translator"})

	const reqID = 42
	ctx := context.Background()

	// Request side: tool "write" has an open map at env → recorded as a
	// KV-array mutation in the registry under this request ID.
	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{{
			Name:       "write",
			Parameters: mustReq(`{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}},"pairs":{"type":"array"}}}`),
		}},
	}
	if _, err := pp.RunBeforeRequest(ctx, reqID, chat, nil); err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}

	// Response side, same request: env is a KV array (registered mutation),
	// pairs is a KV-shaped array the model produced on its own.
	runAs(t, pp, reqID, toolStart(0, "call_reg", "write"))
	runAs(t, pp, reqID, toolDelta(0, `{"env":[{"key":"A","value":"1"}],`))
	runAs(t, pp, reqID, toolDelta(0, `"pairs":[{"key":"k","value":"v"}]}`))

	out := runAs(t, pp, reqID, toolEnd(0))
	if len(out) != 3 || out[0].ToolCallStart == nil || out[1].ToolCallDelta == nil || out[2].ToolCallEnd == nil {
		t.Fatalf("expected [start, delta, end], got %+v", out)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(out[1].ToolCallDelta.ArgumentsDelta), &args); err != nil {
		t.Fatalf("invalid args %q: %v", out[1].ToolCallDelta.ArgumentsDelta, err)
	}
	env, ok := args["env"].(map[string]any)
	if !ok || env["A"] != "1" {
		t.Fatalf("registered path env not reversed: %v", args)
	}
	if _, isArray := args["pairs"].([]any); !isArray {
		t.Fatalf("unregistered pairs was reversed — heuristic ran instead of registry: %v", args)
	}
}

// TestUnregisteredToolNotReversed proves reversal is registry-only. A tool with
// no recorded mutation (nothing was translated on the request side) must have
// its arguments passed through untouched — even when they contain a genuine
// [{"key","value"}] array the agent uses natively. The removed heuristic
// fallback would have rewritten that array into a map and corrupted the call.
func TestUnregisteredToolNotReversed(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "schema_translator")
	pp := newTestPipeline(t, bundles, []string{"schema_translator"})

	// The registry envelope must exist (the stream hook cannot prove
	// pass-through without it): every request-side invocation publishes one,
	// even when nothing changed. A tool with no open maps translates
	// nothing, leaving "emit" unregistered.
	const reqID = 7
	chat := &engine.ChatRequest{Tools: []engine.ToolDef{{Name: "read", Parameters: mustReq(`{"type":"object","properties":{"path":{"type":"string"}}}`)}}}
	if _, err := pp.RunBeforeRequest(context.Background(), reqID, chat, nil); err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	runAs(t, pp, reqID, toolStart(0, "call_native", "emit"))
	runAs(t, pp, reqID, toolDelta(0, `{"tags":[{"key":"k","value":"v"}]}`))

	out := runAs(t, pp, reqID, toolEnd(0))
	if len(out) != 3 || out[0].ToolCallStart == nil || out[1].ToolCallDelta == nil || out[2].ToolCallEnd == nil {
		t.Fatalf("expected [start, delta, end], got %+v", out)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(out[1].ToolCallDelta.ArgumentsDelta), &args); err != nil {
		t.Fatalf("invalid args %q: %v", out[1].ToolCallDelta.ArgumentsDelta, err)
	}
	if _, isArray := args["tags"].([]any); !isArray {
		t.Fatalf("native KV array was reversed on an unregistered tool: %v", args)
	}
}

// TestStreamChaining: schema_translator fans out at ToolCallEnd; the intent
// plugin consumes that fan-out, extracts the intent, and strips the "i" field.
func TestStreamChaining(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "schema_translator")
	requireBundle(t, bundles, "intent")
	pp := newTestPipeline(t, bundles, []string{"schema_translator", "intent"})

	// Translate the schema first (reqID 1, matching run's default) so env's
	// KV-array mutation is registered and reversed via the registry.
	registerEnvMap(t, pp, 1, "write")

	if out := run(t, pp, toolStart(0, "call_2", "write")); len(out) != 0 {
		t.Fatalf("expected the start suppressed (host-side assembly), got %+v", out)
	}
	// Args carry an injected "i" intent plus a KV-array map.
	if out := run(t, pp, toolDelta(0, `{"i":"find the bug",`)); len(out) != 0 {
		t.Fatalf("expected fragment suppressed, got %+v", out)
	}
	if out := run(t, pp, toolDelta(0, `"env":[{"key":"A","value":"1"}]}`)); len(out) != 0 {
		t.Fatalf("expected fragment suppressed, got %+v", out)
	}

	out := run(t, pp, toolEnd(0))
	if len(out) != 3 || out[0].ToolCallStart == nil || out[1].ToolCallDelta == nil || out[2].ToolCallEnd == nil {
		t.Fatalf("expected [start, delta, end], got %+v", out)
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(out[1].ToolCallDelta.ArgumentsDelta), &args); err != nil {
		t.Fatalf("emitted args not valid JSON: %v (%q)", err, out[1].ToolCallDelta.ArgumentsDelta)
	}
	if _, hasI := args["i"]; hasI {
		t.Fatalf(`expected "i" stripped by the intent plugin, got %v`, args)
	}
	env, ok := args["env"].(map[string]any)
	if !ok || env["A"] != "1" {
		t.Fatalf("expected env reversed by schema_translator, got %v", args)
	}
}

// TestIntentRehydratesHistory: the response side strips "i" and caches it;
// on a later request the intent plugin must restore "i" onto that same tool
// call in history (from the cache) so the model sees consistent usage. This
// is the fix for multi-turn intent collapse (the model imitating its own
// "i"-stripped history).
func TestIntentRehydratesHistory(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "intent")
	pp := newTestPipeline(t, bundles, []string{"intent"})

	// Turn 1 response: model emits a tool call carrying "i"; the plugin caches
	// the intent under the tool_call_id and strips it from the emitted args.
	if out := run(t, pp, toolStart(0, "call_hist", "read")); len(out) != 0 {
		t.Fatalf("expected the start suppressed (host-side assembly), got %+v", out)
	}
	if out := run(t, pp, toolDelta(0, `{"i":"where is the retry budget configured","path":"failover.go"}`)); len(out) != 0 {
		t.Fatalf("expected fragment suppressed, got %+v", out)
	}
	out := run(t, pp, toolEnd(0))
	if len(out) != 3 || out[0].ToolCallStart == nil || out[1].ToolCallDelta == nil || out[2].ToolCallEnd == nil {
		t.Fatalf("expected [start, delta, end], got %+v", out)
	}
	var stripped map[string]any
	json.Unmarshal([]byte(out[1].ToolCallDelta.ArgumentsDelta), &stripped)
	if _, hasI := stripped["i"]; hasI {
		t.Fatalf(`turn 1: "i" should be stripped from emitted args, got %v`, stripped)
	}

	// Turn 2 request: the harness replays the (stripped) tool call in history.
	// The intent plugin must re-hydrate "i" from the cache.
	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{{Name: "read", Parameters: mustReq(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
		Messages: []engine.Message{
			{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "trace the retry logic"}}}},
			{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{ID: "call_hist", Name: "read", Arguments: mustReq(`{"path":"failover.go"}`)}}}},
			{Role: engine.RoleTool, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{ToolCallID: "call_hist", Content: []engine.ToolResultContentBlock{{Text: "package proxy ..."}}}}}},
		},
	}
	out2, err := pp.RunBeforeRequest(context.Background(), 2, chat, nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	var histArgs map[string]json.RawMessage
	for _, m := range out2.Messages {
		if m.Role == engine.RoleAssistant && len(m.Blocks) == 1 && m.Blocks[0].ToolUse != nil && m.Blocks[0].ToolUse.ID == "call_hist" {
			histArgs, _, _ = m.Blocks[0].ToolUse.Arguments.DecodeObject()
		}
	}
	if histArgs == nil {
		t.Fatalf("history tool call missing after rehydration: %v", out2.Messages)
	}
	if got := string(histArgs["i"]); got != `"where is the retry budget configured"` {
		t.Fatalf(`"i" not re-hydrated onto history tool call: got %q (args %v)`, got, histArgs)
	}
	if string(histArgs["path"]) != `"failover.go"` {
		t.Fatalf("rehydration clobbered original args: %v", histArgs)
	}
}

// TestIntentFillsUncachedHistory: a history tool call with no cached intent
// (the model organically omitted "i" — nothing to rehydrate) gets a HEURISTIC
// fill by default. History "i" values act as few-shot examples: one "i"-less
// call becomes a self-reinforcing per-tool precedent (measured Jul 18: an
// "i"-less history collapses next-call emission to 0/9; filling the holes
// restores 8/8). The fill is derived per request — never cached, never
// bridged — so the intent cache stays real-captured-only.
func TestIntentFillsUncachedHistory(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "intent")
	store := cache.NewLocalCache(time.Minute)
	pp := newTestPipelineWith(t, bundles, []string{"intent"}, store, nil)

	mkChat := func(userMsg string) *engine.ChatRequest {
		return &engine.ChatRequest{
			Tools: []engine.ToolDef{{Name: "read", Parameters: mustReq(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
			Messages: []engine.Message{
				{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: userMsg}}}},
				{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{ID: "call_never_seen", Name: "read", Arguments: mustReq(`{"path":"x.go"}`)}}}},
				{Role: engine.RoleTool, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{ToolCallID: "call_never_seen", Content: []engine.ToolResultContentBlock{{Text: "..."}}}}}},
			},
		}
	}
	fillOf := func(out *engine.ChatRequest) string {
		t.Helper()
		for _, m := range out.Messages {
			if m.Role == engine.RoleAssistant && len(m.Blocks) == 1 && m.Blocks[0].ToolUse != nil {
				vals, _, _ := m.Blocks[0].ToolUse.Arguments.DecodeObject()
				// DecodeObject returns exact lexemes: a JSON string value is
				// quoted. Unquote for the comparison.
				var i string
				_ = json.Unmarshal(vals["i"], &i)
				return i
			}
		}
		t.Fatalf("history tool call missing: %v", out.Messages)
		return ""
	}

	out, err := pp.RunBeforeRequest(context.Background(), 3, mkChat("trace the retry logic"), nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	fill := fillOf(out)
	if fill == "" {
		t.Fatal("uncached history tool call was not filled")
	}
	if fill != "what x.go shows" {
		t.Fatalf("fill should be a pure function of tool name and args, got %q", fill)
	}

	// Never cached, never bridged: the intent cache must not contain the fill.
	if v, ok := store.Get("intent:call_never_seen"); ok {
		t.Fatalf("fill was bridged into the intent cache: %q", v)
	}
	if v, ok := store.Get(`intentc:["read",{"path":"x.go"}]`); ok {
		t.Fatalf("fill was written under the content key: %q", v)
	}
}

// TestIntentFillOff: fill "off" restores the old behavior — an uncached
// history tool call is left untouched, no spurious "i".
func TestIntentFillOff(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "intent")
	pp := newTestPipelineWith(t, bundles, []string{"intent"}, cache.NewLocalCache(time.Minute),
		map[string]json.RawMessage{"intent": json.RawMessage(`{"fill":"off"}`)})

	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{{Name: "read", Parameters: mustReq(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
		Messages: []engine.Message{
			{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hi"}}}},
			{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{ID: "call_never_seen", Name: "read", Arguments: mustReq(`{"path":"x"}`)}}}},
			{Role: engine.RoleTool, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{ToolCallID: "call_never_seen", Content: []engine.ToolResultContentBlock{{Text: "..."}}}}}},
		},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 3, chat, nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	for _, m := range out.Messages {
		if m.Role == engine.RoleAssistant && len(m.Blocks) == 1 && m.Blocks[0].ToolUse != nil {
			vals, _, _ := m.Blocks[0].ToolUse.Arguments.DecodeObject()
			if _, hasI := vals["i"]; hasI {
				t.Fatalf("fill=off: uncached history tool call should stay untouched: %v", m.Blocks[0].ToolUse.Arguments)
			}
		}
	}
}

// TestIntentBridgesToRequestSideID: harnesses like Claude Code do NOT
// round-trip tool_call_ids — the ID the response side cached under never
// reappears in later request history. Rehydration resolves the intent by
// content key and must BRIDGE it to the request's own tool_call_id, because
// that request-side ID is what the compactors use to look up intents from
// the tool RESULT message (and it stays stable across the session's
// requests; verified in dogfood).
func TestIntentBridgesToRequestSideID(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "intent")
	store := cache.NewLocalCache(time.Minute)
	pp := newTestPipelineWith(t, bundles, []string{"intent"}, store, nil)

	// Turn 1 response: model emits the call under the response-stream ID.
	run(t, pp, toolStart(0, "call_resp_7", "read"))
	run(t, pp, toolDelta(0, `{"i":"where is the retry budget configured","path":"failover.go"}`))
	run(t, pp, toolEnd(0))

	// Turn 2 request: the harness replays the same call under ITS OWN ID.
	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{{Name: "read", Parameters: mustReq(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
		Messages: []engine.Message{
			{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "trace the retry logic"}}}},
			{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{ID: "call_req_42", Name: "read", Arguments: mustReq(`{"path":"failover.go"}`)}}}},
			{Role: engine.RoleTool, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{ToolCallID: "call_req_42", Content: []engine.ToolResultContentBlock{{Text: "package proxy ..."}}}}}},
		},
	}
	if _, err := pp.RunBeforeRequest(context.Background(), 2, chat, nil); err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	got, ok := store.Get("intent:call_req_42")
	if !ok || got != "where is the retry budget configured" {
		t.Fatalf("intent not bridged to request-side ID: got %q (ok=%v)", got, ok)
	}
}

// TestIntentBridgeFeedsKeywordCompactor: the end-to-end #5 regression — with
// reassigned tool_call_ids (the Claude Code path), the keyword compactor's
// unchanged intent:<tool_call_id> lookup must work because the intent plugin
// (running first in the chain) bridges the content-key hit to the request's
// ID before the compactor sees the request.
func TestIntentBridgeFeedsKeywordCompactor(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "intent")
	requireBundle(t, bundles, "keyword_compactor")
	store := cache.NewLocalCache(time.Minute)
	pp := newTestPipelineWith(t, bundles, []string{"intent", "keyword_compactor"}, store,
		map[string]json.RawMessage{"keyword_compactor": json.RawMessage(`{"tool_policies":[{"match":"read","mode":"keyword"}]}`)})

	// Turn 1 response: intent captured under the response-stream ID.
	run(t, pp, toolStart(0, "call_resp_9", "read"))
	run(t, pp, toolDelta(0, `{"i":"where is the retry budget configured","path":"failover.go"}`))
	run(t, pp, toolEnd(0))

	// A big tool result: >50 lines, >2000 chars, with a few lines matching the
	// intent's keywords (retry/budget/configured) buried in filler.
	var lines []string
	for n := 0; n < 120; n++ {
		switch n {
		case 20, 60, 100:
			lines = append(lines, "the retry budget is configured right here, attempt cap and backoff")
		default:
			lines = append(lines, "filler stanza describing nothing of consequence on this line ....")
		}
	}
	big := strings.Join(lines, "\n")

	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{{Name: "read", Parameters: mustReq(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
		Messages: []engine.Message{
			{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "trace the retry logic"}}}},
			{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{ID: "call_req_77", Name: "read", Arguments: mustReq(`{"path":"failover.go"}`)}}}},
			{Role: engine.RoleTool, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{ToolCallID: "call_req_77", Content: []engine.ToolResultContentBlock{{Text: big}}}}}},
			{Role: engine.RoleAssistant, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "I have read the file."}}}},
			{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "Great, now fix it."}}}},
		},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 5, chat, nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	var result string
	for _, m := range out.Messages {
		if m.Role == engine.RoleTool && toolResultID(m) == "call_req_77" {
			result = toolResultText(m)
		}
	}
	if result == "" {
		t.Fatalf("tool result missing from output: %v", out.Messages)
	}
	if len(result) >= len(big)/2 {
		t.Fatalf("tool result was not compacted (%d bytes of %d) — bridge to request-side ID broken?", len(result), len(big))
	}
	if !strings.Contains(result, "retry budget is configured") {
		t.Fatalf("compaction dropped the intent-relevant lines: %q", result)
	}
}

// TestCompactorsRespectToolResultConsumptionBoundary pins the structural
// safety rule from #166 through the real WASM request path. Fresh results,
// including every result in a parallel batch, must reach the model verbatim.
// Once a later assistant message exists, historical results may be compacted,
// including old results in a request that also contains a fresh round.
func TestCompactorsRespectToolResultConsumptionBoundary(t *testing.T) {
	for _, pluginName := range []string{"keyword_compactor", "compactor"} {
		pluginName := pluginName
		t.Run(pluginName, func(t *testing.T) {
			bundles := officialBundlesDir(t)
			requireBundle(t, bundles, pluginName)
			config := map[string]json.RawMessage{
				pluginName: json.RawMessage(`{"tool_policies":[{"match":"read","mode":"deterministic"}]}`),
			}

			for _, tc := range []struct {
				name          string
				messages      []engine.Message
				wantCompacted map[string]bool
			}{
				{
					name: "fresh single result",
					messages: []engine.Message{
						toolCallMessage("fresh-1"),
						toolResultMessage("fresh-1"),
					},
					wantCompacted: map[string]bool{"fresh-1": false},
				},
				{
					name: "fresh parallel batch",
					messages: []engine.Message{
						parallelToolCallMessage("parallel-1", "parallel-2"),
						toolResultMessage("parallel-1"),
						toolResultMessage("parallel-2"),
					},
					wantCompacted: map[string]bool{"parallel-1": false, "parallel-2": false},
				},
				{
					name: "historical result",
					messages: []engine.Message{
						toolCallMessage("old-1"),
						toolResultMessage("old-1"),
						{Role: engine.RoleAssistant, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "I consumed the result."}}}},
						{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "Continue."}}}},
					},
					wantCompacted: map[string]bool{"old-1": true},
				},
				{
					name: "mixed historical and fresh rounds",
					messages: []engine.Message{
						toolCallMessage("old-2"),
						toolResultMessage("old-2"),
						parallelToolCallMessage("fresh-2", "fresh-3"),
						toolResultMessage("fresh-2"),
						toolResultMessage("fresh-3"),
					},
					wantCompacted: map[string]bool{"old-2": true, "fresh-2": false, "fresh-3": false},
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					store := cache.NewLocalCache(time.Minute)
					pp := newTestPipelineWith(t, bundles, []string{pluginName}, store, config)
					out, err := pp.RunBeforeRequest(context.Background(), 1, &engine.ChatRequest{Messages: tc.messages}, nil)
					if err != nil {
						t.Fatalf("RunBeforeRequest: %v", err)
					}

					seen := make(map[string]bool, len(tc.wantCompacted))
					for _, msg := range out.Messages {
						want, tracked := tc.wantCompacted[toolResultID(msg)]
						if msg.Role != engine.RoleTool || !tracked {
							continue
						}
						seen[toolResultID(msg)] = true
						gotText := toolResultText(msg)
						if want && gotText == largeToolResult() {
							t.Errorf("historical result %q was not compacted", toolResultID(msg))
						}
						if !want && gotText != largeToolResult() {
							t.Errorf("fresh result %q changed: got %q", toolResultID(msg), gotText)
						}
					}
					for id := range tc.wantCompacted {
						if !seen[id] {
							t.Errorf("tool result %q missing from output", id)
						}
					}
				})
			}
		})
	}
}

func TestCompactorToolPolicies(t *testing.T) {
	for _, pluginName := range []string{"keyword_compactor", "compactor"} {
		pluginName := pluginName
		t.Run(pluginName, func(t *testing.T) {
			bundles := officialBundlesDir(t)
			requireBundle(t, bundles, pluginName)

			t.Run("explicit first pass deterministic", func(t *testing.T) {
				out := runToolPolicyRequest(t, pluginName,
					`{"tool_policies":[{"match":"WEB_*","mode":"deterministic","first_pass":true,"rerun":"Repeat the original query."}]}`,
					[]engine.Message{toolCallNamedMessage("fresh", "web_search"), toolResultNamedMessage("fresh", "Web_Search")})
				got := toolResultContent(t, out, "fresh")
				if !strings.HasPrefix(got, "[torana-tool-output v1]\n") || !strings.Contains(got, "rerun: Repeat the original query.") {
					t.Fatalf("first-pass deterministic policy did not produce a recoverable marker: %q", got)
				}
			})

			t.Run("resolves missing result tool name", func(t *testing.T) {
				out := runToolPolicyRequest(t, pluginName,
					`{"tool_policies":[{"match":"web_search","mode":"deterministic","first_pass":true}]}`,
					[]engine.Message{toolCallNamedMessage("unnamed", "web_search"), toolResultNamedMessage("unnamed", "")})
				got := toolResultContent(t, out, "unnamed")
				if !strings.Contains(got, "tool: web_search") {
					t.Fatalf("policy did not recover tool name from prior call: %q", got)
				}
			})

			t.Run("source remains exact after aging", func(t *testing.T) {
				messages := sourceHistory("source-exact", 5)
				out := runToolPolicyRequest(t, pluginName,
					`{"tool_policies":[{"match":"read_file","mode":"source","rerun":"Read the file again."}]}`,
					messages)
				if got := toolResultContent(t, out, "source-exact"); got != largeToolResult() {
					t.Fatalf("source mode must fail closed to exact after loop-prone dogfood: %q", got)
				}
			})

			t.Run("unknown and safety sensitive stay exact", func(t *testing.T) {
				for _, toolName := range []string{"unknown_tool", "apply_patch", "git_diff"} {
					config := `{"tool_policies":[{"match":"*","mode":"deterministic","first_pass":true}]}`
					if toolName == "unknown_tool" {
						config = `{"tool_policies":[{"match":"web_search","mode":"deterministic","first_pass":true}]}`
					}
					messages := []engine.Message{
						toolCallNamedMessage("safe", toolName),
						toolResultNamedMessage("safe", toolName),
						{Role: engine.RoleAssistant, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "consumed"}}}},
					}
					out := runToolPolicyRequest(t, pluginName, config, messages)
					if got := toolResultContent(t, out, "safe"); got != largeToolResult() {
						t.Fatalf("tool %q should remain exact, got %q", toolName, got)
					}
				}
			})

			t.Run("replacement stable across raw replays", func(t *testing.T) {
				store := cache.NewLocalCache(time.Minute)
				cfg := map[string]json.RawMessage{pluginName: json.RawMessage(
					`{"tool_policies":[{"match":"web_search","mode":"deterministic","first_pass":true}]}`)}
				pp := newTestPipelineWith(t, bundles, []string{pluginName}, store, cfg)
				request := func() *engine.ChatRequest {
					return &engine.ChatRequest{Messages: []engine.Message{
						toolCallNamedMessage("replay", "web_search"), toolResultNamedMessage("replay", "web_search"),
					}}
				}
				first, err := pp.RunBeforeRequest(context.Background(), 1, request(), nil)
				if err != nil {
					t.Fatalf("first replay: %v", err)
				}
				second, err := pp.RunBeforeRequest(context.Background(), 2, request(), nil)
				if err != nil {
					t.Fatalf("second replay: %v", err)
				}
				if a, b := toolResultContent(t, first, "replay"), toolResultContent(t, second, "replay"); a != b {
					t.Fatalf("canonical replacement changed across replays\nfirst: %q\nsecond: %q", a, b)
				}
			})
		})
	}

	t.Run("model compactor ignores first_pass override", func(t *testing.T) {
		bundles := officialBundlesDir(t)
		requireBundle(t, bundles, "compactor")
		out := runToolPolicyRequest(t, "compactor",
			`{"tool_policies":[{"match":"web_search","mode":"model","first_pass":true}],"expected_applications":5}`,
			[]engine.Message{toolCallNamedMessage("model-fresh", "web_search"), toolResultNamedMessage("model-fresh", "web_search")})
		if got := toolResultContent(t, out, "model-fresh"); got != largeToolResult() {
			t.Fatalf("model compactor summarized before one exact use: %q", got)
		}
	})
}

func runToolPolicyRequest(t *testing.T, pluginName, rawConfig string, messages []engine.Message) *engine.ChatRequest {
	t.Helper()
	store := cache.NewLocalCache(time.Minute)
	pp := newTestPipelineWith(t, officialBundlesDir(t), []string{pluginName}, store,
		map[string]json.RawMessage{pluginName: json.RawMessage(rawConfig)})
	out, err := pp.RunBeforeRequest(context.Background(), 1, &engine.ChatRequest{Messages: messages}, nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	return out
}

func sourceHistory(id string, laterAssistants int) []engine.Message {
	messages := []engine.Message{toolCallNamedMessage(id, "read_file"), toolResultNamedMessage(id, "read_file")}
	for i := 0; i < laterAssistants; i++ {
		messages = append(messages,
			engine.Message{Role: engine.RoleAssistant, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "consumed"}}}},
			engine.Message{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "continue"}}}})
	}
	return messages
}

func toolUseBlocks(m engine.Message) []*engine.ToolUseBlock {
	var out []*engine.ToolUseBlock
	for _, b := range m.Blocks {
		if b.ToolUse != nil {
			out = append(out, b.ToolUse)
		}
	}
	return out
}

func toolResultID(m engine.Message) string {
	for _, b := range m.Blocks {
		if b.ToolResult != nil {
			return b.ToolResult.ToolCallID
		}
	}
	return ""
}

func toolResultText(m engine.Message) string {
	var out string
	for _, b := range m.Blocks {
		if b.ToolResult != nil {
			for _, c := range b.ToolResult.Content {
				out += c.Text
			}
		}
	}
	return out
}

func toolCallNamedMessage(id, name string) engine.Message {
	return engine.Message{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{ID: id, Name: name}}}}
}

func toolResultNamedMessage(id, name string) engine.Message {
	return engine.Message{Role: engine.RoleTool, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{ToolCallID: id, ToolName: name, Content: []engine.ToolResultContentBlock{{Text: largeToolResult()}}}}}}
}

func toolResultContent(t *testing.T, req *engine.ChatRequest, id string) string {
	t.Helper()
	for _, message := range req.Messages {
		if message.Role == engine.RoleTool && toolResultID(message) == id {
			return toolResultText(message)
		}
	}
	t.Fatalf("tool result %q missing", id)
	return ""
}

func toolCallMessage(ids ...string) engine.Message {
	return parallelToolCallMessage(ids...)
}

func parallelToolCallMessage(ids ...string) engine.Message {
	blocks := make([]engine.Block, 0, len(ids))
	for _, id := range ids {
		blocks = append(blocks, engine.Block{ToolUse: &engine.ToolUseBlock{ID: id, Name: "read"}})
	}
	return engine.Message{Role: engine.RoleAssistant, Blocks: blocks}
}

func toolResultMessage(id string) engine.Message {
	return engine.Message{Role: engine.RoleTool, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{ToolCallID: id, ToolName: "read", Content: []engine.ToolResultContentBlock{{Text: largeToolResult()}}}}}}
}

func largeToolResult() string {
	var lines []string
	for i := 0; i < 80; i++ {
		if i == 40 {
			lines = append(lines, "needle: exact relevant line")
			continue
		}
		lines = append(lines, "filler output that is intentionally long and repetitive ................")
	}
	return strings.Join(lines, "\n")
}

// TestIntentInjectsNoSyntheticMessages: the intent plugin must teach the "i"
// convention WITHOUT adding messages — a fake conversation is
// indistinguishable from real history and contaminates model behavior
// (verbatim intent leaks, topic-anchored refusals; caught live and in the
// Jul 16 experiments). The example transcript lives in the system prompt.
// Also pins the historical invariant: assistant tool_calls must stay
// immediately followed by their tool results (strict providers 400 otherwise).
func TestIntentInjectsNoSyntheticMessages(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "intent")
	pp := newTestPipeline(t, bundles, []string{"intent"})

	chat := &engine.ChatRequest{
		Messages: []engine.Message{
			{Role: engine.RoleSystem, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "sys"}}}},
			{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "do the thing"}}}},
			{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{ID: "call_real_1", Name: "read", Arguments: mustReq(`{"path":"x"}`)}}}},
			{Role: engine.RoleTool, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{ToolCallID: "call_real_1", Content: []engine.ToolResultContentBlock{{Text: "result"}}}}}},
		},
		Tools: []engine.ToolDef{{Name: "read", Parameters: mustReq(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 77, chat, nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}

	// Every assistant message carrying tool_calls must be immediately
	// followed by tool messages answering each of its call IDs.
	for i, m := range out.Messages {
		if m.Role != engine.RoleAssistant || len(toolUseBlocks(m)) == 0 {
			continue
		}
		want := map[string]bool{}
		for _, tc := range toolUseBlocks(m) {
			want[tc.ID] = true
		}
		for j := i + 1; j < len(out.Messages) && len(want) > 0; j++ {
			if out.Messages[j].Role != engine.RoleTool {
				t.Fatalf("assistant tool_calls at %d followed by %q at %d (unanswered: %v)\nsequence: %v",
					i, out.Messages[j].Role, j, want, roles(out.Messages))
			}
			delete(want, toolResultID(out.Messages[j]))
		}
		if len(want) > 0 {
			t.Fatalf("assistant tool_calls at %d never answered: %v", i, want)
		}
	}

	// No synthetic messages: exactly the 4 originals, in order.
	if len(out.Messages) != 4 {
		t.Fatalf("intent plugin added messages (want 4, got %d): %v", len(out.Messages), roles(out.Messages))
	}
	for _, m := range out.Messages {
		for _, tc := range toolUseBlocks(m) {
			if strings.Contains(tc.ID, "fewshot") {
				t.Fatalf("synthetic few-shot message injected: %v", roles(out.Messages))
			}
		}
	}
	// The convention (with its example transcript) rides the system prompt.
	if !strings.Contains(engineText(out.Messages[0]), `"i"`) {
		t.Fatalf("system prompt missing the intent addendum: %q", engineText(out.Messages[0]))
	}
}

func roles(msgs []engine.Message) []string {
	var out []string
	for _, m := range msgs {
		r := string(m.Role)
		if len(toolUseBlocks(m)) > 0 {
			r += "(tool_calls)"
		}
		out = append(out, r)
	}
	return out
}

// TestIntentNativeIEnrichesDescriptionOnly: a harness whose tools natively
// declare "i" (omp adopted the intent field itself) keeps its structural
// contract — required/optionality and additionalProperties untouched, and
// the response side CAPTURES the value but never strips it (the harness's
// tools expect the parameter). Only the description is upgraded to the
// example-carrying form: advisory prose, not contract, and omp's native
// "concise intent" produced action-labels that starve the compactors.
func TestIntentNativeIEnrichesDescriptionOnly(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "intent")
	store := cache.NewLocalCache(time.Minute)
	pp := newTestPipelineWith(t, bundles, []string{"intent"}, store, nil)

	nativeParams := mustReq(`{"type":"object","properties":{"i":{"type":"string","description":"omp's own intent semantics"},"path":{"type":"string"}},"required":["path"]}`)
	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{
			{Name: "read", Parameters: nativeParams},
			{Name: "plain", Parameters: mustReq(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
		},
		Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "go"}}}}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 6, chat, nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	if out == nil {
		out = chat
	}
	var native, plain map[string]json.RawMessage
	for _, tool := range out.Tools {
		switch tool.Name {
		case "read":
			native, _, _ = tool.Parameters.DecodeObject()
		case "plain":
			plain, _, _ = tool.Parameters.DecodeObject()
		}
	}
	// Native tool: description upgraded, structure preserved.
	var props map[string]json.RawMessage
	json.Unmarshal(native["properties"], &props)
	var iProp map[string]json.RawMessage
	json.Unmarshal(props["i"], &iProp)
	desc := string(iProp["description"])
	if !strings.Contains(desc, "NOT the action taken") {
		t.Fatalf("native i description not enriched: %q", desc)
	}
	if req := string(native["required"]); req != `["path"]` {
		t.Fatalf("native tool required list mutated: %v", req)
	}
	if _, ok := native["additionalProperties"]; ok {
		t.Fatalf("additionalProperties bolted onto native-i tool")
	}
	// The plain tool still gets the injection.
	var pprops map[string]json.RawMessage
	json.Unmarshal(plain["properties"], &pprops)
	if _, ok := pprops["i"]; !ok {
		t.Fatalf("plain tool did not get i injected: %v", plain)
	}

	// Response side (same request ID — hadI is request-scoped meta):
	// capture but DO NOT strip for the native tool.
	runAs(t, pp, 6, toolStart(0, "call_native", "read"))
	runAs(t, pp, 6, toolDelta(0, `{"i":"find the retry budget","path":"failover.go"}`))
	out2 := runAs(t, pp, 6, toolEnd(0))
	if len(out2) != 3 || out2[0].ToolCallStart == nil || out2[1].ToolCallDelta == nil || out2[2].ToolCallEnd == nil {
		t.Fatalf("expected [start, delta, end], got %+v", out2)
	}
	var args map[string]any
	json.Unmarshal([]byte(out2[1].ToolCallDelta.ArgumentsDelta), &args)
	if args["i"] != "find the retry budget" {
		t.Fatalf(`native "i" must NOT be stripped, got %v`, args)
	}
	if v, ok := store.Get("intent:call_native"); !ok || v != "find the retry budget" {
		t.Fatalf("native i not captured into cache: %q ok=%v", v, ok)
	}
}

// TestStreamBlockTopologySurvivesPlugins: a text block passed through a plugin
// (which only touches TextDelta) must come back with its block topology
// intact — BlockStart stays BlockStart and BlockStop stays BlockStop. The v2
// wire's stop carries only an index; the host's per-request BlockKindTracker
// is what keeps a plugin-passed text block's stop from being misread as a tool
// call's end (which would leave the serializer with an open part it never
// flushes — the exact loss this preserves).
func TestStreamBlockTopologySurvivesPlugins(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-stream-mutator/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-stream-mutator"})

	text := "hello"
	out := run(t, pp, engine.StreamEvent{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}})
	if len(out) != 1 || out[0].BlockStart == nil || out[0].BlockStart.Kind != engine.BlockKindText || out[0].BlockStart.Index != 0 {
		t.Fatalf("BlockStart did not survive plugins: %+v", out)
	}

	out = run(t, pp, engine.StreamEvent{TextDelta: &text})
	if len(out) != 1 || out[0].TextDelta == nil || *out[0].TextDelta != "hello" {
		t.Fatalf("TextDelta did not survive plugins: %+v", out)
	}

	out = run(t, pp, engine.StreamEvent{BlockStop: &engine.BlockStop{Index: 0}})
	if len(out) != 1 || out[0].BlockStop == nil || out[0].BlockStop.Index != 0 {
		t.Fatalf("BlockStop became %+v — the open-block kind must survive plugins", out)
	}
	if out[0].ToolCallEnd != nil {
		t.Fatalf("text block stop misread as ToolCallEnd: %+v", out[0].ToolCallEnd)
	}

	// A tool block still resolves its stop to ToolCallEnd. Indexes are unique
	// per streamed message, so the tool block takes the NEXT index (1) rather
	// than reusing the text block's 0.
	out = run(t, pp, toolStart(1, "call_1", "read_file"))
	if len(out) != 1 || out[0].ToolCallStart == nil {
		t.Fatalf("ToolCallStart did not survive plugins: %+v", out)
	}
	out = run(t, pp, toolEnd(1))
	if len(out) != 1 || out[0].ToolCallEnd == nil || out[0].ToolCallEnd.Index != 1 {
		t.Fatalf("tool stop did not survive plugins as ToolCallEnd: %+v", out)
	}
	if out[0].BlockStop != nil {
		t.Fatalf("tool stop misread as BlockStop: %+v", out[0].BlockStop)
	}
}

// flatEvent is the reduced shape asserted after a pipeline pass: which kind
// of event, at which block index, with which payload fields.
type flatEvent struct {
	kind  string // start | delta | end | finish
	index int
	id    string
	name  string
	delta string
}

func flatStream(events []engine.StreamEvent) []flatEvent {
	out := make([]flatEvent, 0, len(events))
	for _, ev := range events {
		switch {
		case ev.ToolCallStart != nil:
			out = append(out, flatEvent{kind: "start", index: ev.ToolCallStart.Index, id: ev.ToolCallStart.ID, name: ev.ToolCallStart.Name})
		case ev.ToolCallDelta != nil:
			out = append(out, flatEvent{kind: "delta", index: ev.ToolCallDelta.Index, delta: ev.ToolCallDelta.ArgumentsDelta})
		case ev.ToolCallEnd != nil:
			out = append(out, flatEvent{kind: "end", index: ev.ToolCallEnd.Index})
		case ev.FinishReason != "":
			out = append(out, flatEvent{kind: "finish", delta: ev.FinishReason})
		case ev.Error != nil:
			out = append(out, flatEvent{kind: "error", delta: ev.Error.Message})
		}
	}
	return out
}

// pipeStream pushes every parsed engine event of one response stream through
// the plugin pipeline under one request ID (one tracker = one streamed
// message) and returns what came back.
func pipeStream(t *testing.T, pp *PluginPipeline, reqID uint64, events []engine.StreamEvent) []engine.StreamEvent {
	t.Helper()
	var got []engine.StreamEvent
	for _, ev := range events {
		out, err := pp.RunOnStreamChunk(context.Background(), reqID, &ev)
		if err != nil {
			t.Fatalf("RunOnStreamChunk: %v", err)
		}
		got = append(got, out...)
	}
	return got
}

// TestOpenAIParallelChatSurvivesPlugins: a real OpenAI Chat SSE stream with
// two parallel tool calls — interleaved starts and argument deltas, then both
// stops — parsed into engine events and pushed through the real plugin
// pipeline must come back with every start/delta/stop intact at its own
// index, and must NOT error. The round-4 tracker allows multiple tool blocks
// open at once (the Chat adapter emits ToolCallEnd for both calls only at
// finish_reason="tool_calls", so both are open simultaneously), which the
// round-3 single-open rule would have rejected at the plugin boundary.
func TestOpenAIParallelChatSurvivesPlugins(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-inert-a/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-inert-a"})

	sse := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_weather","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_time","type":"function","function":{"name":"get_time","arguments":""}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"tz\":"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Pune\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"UTC\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n")

	events := drainChunks((&openai.StreamAdapter{}).ParseStream(strings.NewReader(sse)))
	if len(events) != 9 {
		t.Fatalf("parsed %d events, want 9: %+v", len(events), flatStream(events))
	}

	got := flatStream(pipeStream(t, pp, 300, events))
	want := []flatEvent{
		{kind: "start", index: 0, id: "call_weather", name: "get_weather"},
		{kind: "start", index: 1, id: "call_time", name: "get_time"},
		{kind: "delta", index: 0, delta: `{"city":`},
		{kind: "delta", index: 1, delta: `{"tz":`},
		{kind: "delta", index: 0, delta: `"Pune"}`},
		{kind: "delta", index: 1, delta: `"UTC"}`},
		{kind: "end", index: 0},
		{kind: "end", index: 1},
		{kind: "finish", delta: "tool_calls"},
	}
	if len(got) != len(want) {
		t.Fatalf("pipeline returned %d events, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("event %d = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestOpenAIResponsesParallelSurvivesPlugins: the Responses API's interleaved
// parallel calls — two function_call items added back to back, argument
// deltas alternating by item, and each call completed at its own time — must
// survive parse → pb → plugin → engine without error. The second call's stop
// arrives while the first is still open (non-ascending stop order), which is
// exactly the concurrency the round-4 tracker permits.
func TestOpenAIResponsesParallelSurvivesPlugins(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-inert-a/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-inert-a"})

	sse := strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"id":"item_weather","type":"function_call","name":"weather","call_id":"call_weather"}}`,
		`data: {"type":"response.output_item.added","item":{"id":"item_time","type":"function_call","name":"time","call_id":"call_time"}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_time","delta":"{\"tz\":"}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_weather","delta":"{\"city\":"}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_time","delta":"\"UTC\"}"}`,
		`data: {"type":"response.function_call_arguments.done","item_id":"item_time"}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_weather","delta":"\"Pune\"}"}`,
		`data: {"type":"response.function_call_arguments.done","item_id":"item_weather"}`,
	}, "\n")

	events := drainChunks((&openai.StreamAdapter{}).ParseStream(strings.NewReader(sse)))
	if len(events) != 8 {
		t.Fatalf("parsed %d events, want 8: %+v", len(events), flatStream(events))
	}

	got := flatStream(pipeStream(t, pp, 301, events))
	want := []flatEvent{
		{kind: "start", index: 0, id: "call_weather", name: "weather"},
		{kind: "start", index: 1, id: "call_time", name: "time"},
		{kind: "delta", index: 1, delta: `{"tz":`},
		{kind: "delta", index: 0, delta: `{"city":`},
		{kind: "delta", index: 1, delta: `"UTC"}`},
		{kind: "end", index: 1},
		{kind: "delta", index: 0, delta: `"Pune"}`},
		{kind: "end", index: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("pipeline returned %d events, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("event %d = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestStreamLosslessThroughPipeline is the end-to-end claim of the lossless IR
// work: a Code Assist wire stream parsed into engine events, run through a
// plugin, and re-serialized must reproduce the SAME part structure — the
// current-block signature stays beside its text in ONE part, and a trailing
// standalone signature stays its own empty-text part. Before block events
// existed, the pipeline's stateless stop conversion turned a text block's stop
// into ToolCallEnd, which the serializer could not flush.
func TestStreamLosslessThroughPipeline(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-stream-mutator/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-stream-mutator"})

	// Each subtest is ONE response stream, and the host tracks block topology
	// per REQUEST — one response stream is one streamed message, and indexes
	// stay unique within it. Distinct request IDs keep the two subtests'
	// streams from sharing a tracker.
	for i, tc := range []struct {
		name string
		wire string
		want []string // expected serialized raw part JSON, in order
	}{
		{
			name: "current text signature stays one part",
			wire: `data: {"response":{"candidates":[{"content":{"role":"model","parts":[` +
				`{"text":"A","thoughtSignature":"S"}` +
				`]}}]}}`,
			want: []string{`{"text":"A","thoughtSignature":"S"}`},
		},
		{
			name: "trailing standalone stays its own part",
			wire: `data: {"response":{"candidates":[{"content":{"role":"model","parts":[` +
				`{"text":"A"},{"text":"","thoughtSignature":"S"}` +
				`]}}]}}`,
			want: []string{`{"text":"A"}`, `{"text":"","thoughtSignature":"S"}`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reqID := uint64(200 + i)
			sa := &gemini.StreamAdapter{Wrapped: true}
			events := drainChunks(sa.ParseStream(strings.NewReader(tc.wire)))

			var piped []engine.StreamEvent
			for _, ev := range events {
				out, err := pp.RunOnStreamChunk(context.Background(), reqID, &ev)
				if err != nil {
					t.Fatalf("RunOnStreamChunk: %v", err)
				}
				piped = append(piped, out...)
			}

			var buf strings.Builder
			if err := sa.SerializeStream(context.Background(), &buf, replayEngine(piped)); err != nil {
				t.Fatalf("SerializeStream: %v", err)
			}

			parts := rawPartsOf(t, buf.String())
			if len(parts) != len(tc.want) {
				t.Fatalf("serialized %d parts, want %d: %v", len(parts), len(tc.want), parts)
			}
			for i, want := range tc.want {
				got := string(parts[i])
				if got != want {
					t.Errorf("part %d = %s, want %s", i, got, want)
				}
			}
		})
	}
}

// drainChunks collects a ParseStream channel into a slice.
func drainChunks(ch <-chan engine.StreamEvent) []engine.StreamEvent {
	var out []engine.StreamEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// replayEngine feeds events back one at a time.
func replayEngine(events []engine.StreamEvent) <-chan engine.StreamEvent {
	ch := make(chan engine.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch
}

// rawPartsOf re-parses serialized SSE and returns the parts of the first
// candidate's content as compact raw JSON (key presence included).
func rawPartsOf(t *testing.T, output string) []json.RawMessage {
	t.Helper()
	var parts []json.RawMessage
	for _, block := range strings.Split(output, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		data, ok := strings.CutPrefix(block, "data:")
		if !ok {
			t.Fatalf("frame missing data: prefix: %q", block)
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &frame); err != nil {
			t.Fatalf("frame not valid JSON: %v (%q)", err, block)
		}
		if resp, ok := frame["response"].(map[string]any); ok {
			frame = resp
		}
		cands, _ := frame["candidates"].([]any)
		if len(cands) == 0 {
			continue
		}
		content, _ := cands[0].(map[string]any)["content"].(map[string]any)
		if content == nil {
			continue
		}
		ps, _ := content["parts"].([]any)
		for _, p := range ps {
			raw, _ := json.Marshal(p)
			parts = append(parts, raw)
		}
	}
	return parts
}
