package plugin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/pluginstate"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

// The tier selector is the riskiest plugin in the set despite being the
// simplest, because it mutates the cached prefix. A bug here does not merely
// fail to save money — it invalidates the cache entry on every turn and costs
// more than doing nothing at all.
//
// The marker it writes lives inside the cache breakpoint, so changing the
// marker changes the prefix bytes. These tests exist to pin the one property
// that keeps it safe: once a tier is chosen for a prefix, it never changes.

// tierSelectorPipeline wires a pipeline with the durable state and pricing the
// plugin needs, since without them it correctly declines to act at all.
func tierSelectorPipeline(t *testing.T, refreshOnRead bool, readRate, writeRate float64) (*PluginPipeline, *pluginstate.Store) {
	t.Helper()
	dir := officialBundlesDir(t)
	requireBundle(t, dir, "cache_tier_selector")

	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { rt.Close() })

	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatalf("pluginstate.New: %v", err)
	}
	rt.StateGetFunc = state.Get
	rt.StateSetFunc = state.Set
	rt.StateKeysFunc = state.Keys
	rt.CachePricingFunc = func(_ context.Context, _ string) wasm.ExtensionResult {
		b, _ := json.Marshal(map[string]any{
			"status":                   "ok",
			"cache_read_usd_per_mtok":  readRate,
			"cache_write_usd_per_mtok": writeRate,
			"write_read_ratio":         writeRate / readRate,
			"break_even_refreshes":     int(writeRate/readRate) - 1,
			"refresh_on_read":          refreshOnRead,
			"shortest_ttl_seconds":     300,
			"warm_interval_seconds":    240,
			"tiers": []any{
				map[string]any{"ttl_seconds": 300, "write_multiplier": 1.25,
					"marker": map[string]any{"type": "ephemeral"}},
				map[string]any{"ttl_seconds": 3600, "write_multiplier": 2.0,
					"marker": map[string]any{"type": "ephemeral", "ttl": "1h"}},
			},
		})
		return wasm.ExtensionValue(b)
	}

	pp, err := NewPipeline(rt, PluginConfig{
		Dir:             dir,
		Order:           []string{"cache_tier_selector"},
		AllowUnapproved: true,
		Config: map[string]json.RawMessage{
			// A 1-second threshold so the test does not have to simulate a
			// realistic pause to exercise the long-tier path.
			"cache_tier_selector": json.RawMessage(`{"mode":"auto","min_gap_seconds_for_long_tier":1}`),
		},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if pp.Len() != 1 {
		t.Fatalf("plugin not loaded (loaded=%d)", pp.Len())
	}
	return pp, state
}

// tierSelectorPipelineWithMode builds the pipeline with an explicit mode, for
// tests that need the plugin to actually act rather than correctly decline.
func tierSelectorPipelineWithMode(t *testing.T, mode string) *PluginPipeline {
	t.Helper()
	dir := officialBundlesDir(t)
	requireBundle(t, dir, "cache_tier_selector")

	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { rt.Close() })

	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatalf("pluginstate.New: %v", err)
	}
	rt.StateGetFunc = state.Get
	rt.StateSetFunc = state.Set
	rt.CachePricingFunc = func(_ context.Context, _ string) wasm.ExtensionResult {
		return wasm.ExtensionValue([]byte(`{"status":"ok","refresh_on_read":true,"shortest_ttl_seconds":300,` +
			`"write_read_ratio":12.5,"break_even_refreshes":11,` + anthropicTiers + `}`))
	}

	pp, err := NewPipeline(rt, PluginConfig{
		Dir:             dir,
		Order:           []string{"cache_tier_selector"},
		AllowUnapproved: true,
		Config: map[string]json.RawMessage{
			"cache_tier_selector": json.RawMessage(`{"mode":"` + mode + `"}`),
		},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return pp
}

// anthropicTiers is the two-lifetime menu the plugin chooses between. It has to
// be present for the plugin to do anything: since it stopped hard-coding one
// provider's markers it reads them from here, and a provider selling a single
// lifetime correctly has no decision to make.
const anthropicTiers = `"tiers":[` +
	`{"ttl_seconds":300,"write_multiplier":1.25,"marker":{"type":"ephemeral"}},` +
	`{"ttl_seconds":3600,"write_multiplier":2.0,"marker":{"type":"ephemeral","ttl":"1h"}}]`

// tierRequest is a conversation with a breakpoint after the system prompt.
func tierRequest(provider string) *engine.ChatRequest {
	return &engine.ChatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []engine.Message{
			{Role: engine.RoleSystem, Content: "You are a coding agent.",
				CacheControl: map[string]any{"type": "ephemeral"}},
			{Role: engine.RoleUser, Content: "refactor the loader"},
		},
		ToranaMeta: map[string]any{
			"_provider":        provider,
			"_conversation_id": "conv-a3f9",
		},
	}
}

func markerOf(t *testing.T, chat *engine.ChatRequest) map[string]any {
	t.Helper()
	for _, m := range chat.Messages {
		if len(m.CacheControl) > 0 {
			return m.CacheControl
		}
	}
	return nil
}

func markerJSON(t *testing.T, chat *engine.ChatRequest) string {
	t.Helper()
	b, err := json.Marshal(markerOf(t, chat))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestTierDecisionIsStickyAcrossTurns is the test that matters most. If the
// plugin flips the marker mid-conversation, it invalidates the entry it is
// protecting and pays a fresh cache write for the privilege — turning a
// cost-saving plugin into a cost-increasing one, silently.
//
// It runs in mode "long" deliberately. In "auto" the plugin correctly leaves
// rapid turns alone, so the assertion would hold whether or not stickiness
// worked — a test that passes both ways. Forcing the tier makes the plugin
// actually rewrite the marker, which is the only state from which stickiness
// can fail.
func TestTierDecisionIsStickyAcrossTurns(t *testing.T) {
	pp := tierSelectorPipelineWithMode(t, "long")
	ctx := context.Background()

	var markers []string
	for i := uint64(1); i <= 5; i++ {
		out, err := pp.RunBeforeRequest(ctx, i, tierRequest("anth"), nil)
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		markers = append(markers, markerJSON(t, out))
	}

	// Guard the guard: if the plugin stopped rewriting the marker entirely,
	// every turn would trivially agree and this test would silently stop
	// testing anything.
	original := markerJSON(t, tierRequest("anth"))
	if markers[0] == original {
		t.Fatalf("the plugin never applied a tier (marker stayed %s), so stickiness is untested", original)
	}

	for i, got := range markers {
		if got != markers[0] {
			t.Fatalf("marker changed on turn %d: %s then %s — a flipped marker "+
				"invalidates the cache entry and pays a fresh write", i+1, markers[0], got)
		}
	}
}

// TestAutoModeLeavesBusyConversationsAlone is the complement: a conversation
// with no observed pause has nothing to gain from a longer lifetime, and buying
// one costs strictly more.
func TestAutoModeLeavesBusyConversationsAlone(t *testing.T) {
	pp, _ := tierSelectorPipeline(t, true, 0.3, 3.75)

	in := tierRequest("anth")
	before := markerJSON(t, in)
	out, err := pp.RunBeforeRequest(context.Background(), 1, in, nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	if got := markerJSON(t, out); got != before {
		t.Errorf("auto mode upgraded a conversation with no observed idle gap: %s", got)
	}
}

// TestUnknownPricingLeavesRequestAlone — a plugin that cannot price its action
// must not take it. Guessing a tier spends the operator's money on a hunch.
func TestUnknownPricingLeavesRequestAlone(t *testing.T) {
	dir := officialBundlesDir(t)
	requireBundle(t, dir, "cache_tier_selector")

	rt := wasm.NewRuntime(context.Background())
	defer rt.Close()
	state, _ := pluginstate.New(pluginstate.Options{})
	rt.StateGetFunc = state.Get
	rt.StateSetFunc = state.Set
	rt.CachePricingFunc = func(_ context.Context, _ string) wasm.ExtensionResult {
		return wasm.ExtensionValue([]byte(`{"status":"unavailable","reason":"no_pricing_configured"}`))
	}

	pp, err := NewPipeline(rt, PluginConfig{
		Dir:             dir,
		Order:           []string{"cache_tier_selector"},
		AllowUnapproved: true,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	in := tierRequest("anth")
	before := markerJSON(t, in)
	out, err := pp.RunBeforeRequest(context.Background(), 1, in, nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	if got := markerJSON(t, out); got != before {
		t.Errorf("marker changed to %s without pricing — the plugin acted on a guess", got)
	}
}

// TestNoBreakpointIsNotTouched — deciding that something *should* be cached is
// not this plugin's judgement to make. It only chooses between lifetimes for a
// breakpoint someone else placed.
func TestNoBreakpointIsNotTouched(t *testing.T) {
	pp, _ := tierSelectorPipeline(t, true, 0.3, 3.75)

	in := &engine.ChatRequest{
		Model:      "claude-sonnet-4-5",
		Messages:   []engine.Message{{Role: engine.RoleUser, Content: "hello"}},
		ToranaMeta: map[string]any{"_provider": "anth", "_conversation_id": "conv-x"},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 1, in, nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	for i, m := range out.Messages {
		if len(m.CacheControl) > 0 {
			t.Errorf("plugin added a breakpoint at message %d that nobody asked for", i)
		}
	}
}

// TestOffModeDoesNothing — the escape hatch has to actually work, since it is
// what an operator reaches for when they suspect this plugin of misbehaving.
func TestOffModeDoesNothing(t *testing.T) {
	dir := officialBundlesDir(t)
	requireBundle(t, dir, "cache_tier_selector")

	rt := wasm.NewRuntime(context.Background())
	defer rt.Close()
	state, _ := pluginstate.New(pluginstate.Options{})
	rt.StateGetFunc = state.Get
	rt.StateSetFunc = state.Set
	rt.CachePricingFunc = func(_ context.Context, _ string) wasm.ExtensionResult {
		return wasm.ExtensionValue([]byte(`{"status":"ok","refresh_on_read":true,"shortest_ttl_seconds":300,"write_read_ratio":12.5,` + anthropicTiers + `}`))
	}

	pp, err := NewPipeline(rt, PluginConfig{
		Dir:             dir,
		Order:           []string{"cache_tier_selector"},
		AllowUnapproved: true,
		Config: map[string]json.RawMessage{
			"cache_tier_selector": json.RawMessage(`{"mode":"off"}`),
		},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	in := tierRequest("anth")
	before := markerJSON(t, in)
	out, err := pp.RunBeforeRequest(context.Background(), 1, in, nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	if got := markerJSON(t, out); got != before {
		t.Errorf("mode=off still changed the marker to %s", got)
	}
}

// TestDecisionSurvivesRestart — the decision lives in durable state precisely
// so a redeploy does not re-decide and invalidate every live cache entry.
func TestDecisionSurvivesRestart(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "cache_tier_selector")

	stateDir := t.TempDir()
	build := func() *PluginPipeline {
		rt := wasm.NewRuntime(context.Background())
		t.Cleanup(func() { rt.Close() })
		state, err := pluginstate.New(pluginstate.Options{Path: stateDir + "/state.json"})
		if err != nil {
			t.Fatalf("pluginstate.New: %v", err)
		}
		rt.StateGetFunc = state.Get
		rt.StateSetFunc = state.Set
		rt.CachePricingFunc = func(_ context.Context, _ string) wasm.ExtensionResult {
			return wasm.ExtensionValue([]byte(`{"status":"ok","refresh_on_read":true,"shortest_ttl_seconds":300,"write_read_ratio":12.5,"break_even_refreshes":11,` + anthropicTiers + `}`))
		}
		pp, err := NewPipeline(rt, PluginConfig{
			Dir:             bundles,
			Order:           []string{"cache_tier_selector"},
			AllowUnapproved: true,
			Config: map[string]json.RawMessage{
				"cache_tier_selector": json.RawMessage(`{"mode":"long"}`),
			},
		})
		if err != nil {
			t.Fatalf("NewPipeline: %v", err)
		}
		return pp
	}

	first, err := build().RunBeforeRequest(context.Background(), 1, tierRequest("anth"), nil)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := build().RunBeforeRequest(context.Background(), 2, tierRequest("anth"), nil)
	if err != nil {
		t.Fatalf("after restart: %v", err)
	}

	if a, b := markerJSON(t, first), markerJSON(t, second); a != b {
		t.Errorf("a restart changed the marker: %s then %s — every live cache entry would be invalidated", a, b)
	}
}
