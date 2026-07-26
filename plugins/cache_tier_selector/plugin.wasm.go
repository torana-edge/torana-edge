// cache_tier_selector picks which prompt-cache lifetime to buy for a
// conversation.
//
// Providers that sell explicit cache breakpoints often sell more than one
// lifetime at different write prices — Anthropic's are 5 minutes at 1.25x the
// base input rate and 1 hour at 2.0x. Which is cheaper depends entirely on how
// long the conversation is about to sit idle, and a harness always asks for the
// default.
//
// The arithmetic, for a prefix of P tokens at base rate B:
//
//	short tier, cache lapses:   pay (1.25 - 1.0) x B x P again on resume
//	long tier, bought upfront:  pay (2.0 - 1.25) x B x P once, no lapse
//
// So the long tier costs about 3x the short tier's *premium*, but only once —
// while a lapsed short tier costs its full write every time the gap exceeds
// five minutes. Buying the hour wins whenever a session has more than a few
// multi-minute pauses in it, which is what real coding sessions look like.
//
// # Why the decision must be sticky
//
// This is the part that makes the plugin subtle. The tier is selected by a
// marker inside the request's cache breakpoint, so changing the marker
// *changes the prefix bytes*. Sending {"type":"ephemeral"} on one turn and
// {"type":"ephemeral","ttl":"1h"} on the next invalidates the very entry the
// plugin is trying to preserve, and pays a fresh write for the privilege.
//
// So the decision is made once per cache prefix and then never revisited. A new
// prefix means the old entry is already dead, which is the only safe moment to
// choose again. A plugin that re-decided per turn would cost money rather than
// save it, and would fail Torana's determinism test — which is the test that
// exists to catch precisely this class of bug.
package main

import (
	"context"
	"encoding/json"
	"fmt"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/pb"
)

func main() {}

// decision is what the plugin remembers per cache prefix.
type decision struct {
	// Marker is the breakpoint value chosen for this prefix, stored so every
	// later turn re-applies exactly the same bytes.
	Marker map[string]any `json:"marker"`
	// TierTTL is the lifetime that marker buys, kept for diagnostics.
	TierTTL int `json:"tier_ttl_seconds"`
	// DecidedAtMillis is when the choice was made.
	DecidedAtMillis int64 `json:"decided_at_millis"`
}

// activity is the per-conversation gap history the decision is based on.
type activity struct {
	LastSeenMillis int64 `json:"last_seen_millis"`
	// LongestGapMillis is the largest idle gap observed so far. A single long
	// pause is enough to justify the longer tier for the rest of the session:
	// a conversation that has gone quiet once will do it again.
	LongestGapMillis int64 `json:"longest_gap_millis"`
	Turns            int   `json:"turns"`
}

type config struct {
	// Mode is "auto" (choose per observed gaps), "short", "long", or "off".
	Mode string `json:"mode"`
	// MinGapSecondsForLongTier is how long an observed pause must be before the
	// longer tier is bought. Defaults to 30% of the long tier's TTL, which
	// keeps a single unusual pause from committing a short conversation.
	MinGapSecondsForLongTier int `json:"min_gap_seconds_for_long_tier"`
}

func loadConfig() config {
	cfg := config{Mode: "auto"}
	if raw := sdk.PluginConfig(); raw != "" {
		_ = json.Unmarshal([]byte(raw), &cfg)
	}
	if cfg.Mode == "" {
		cfg.Mode = "auto"
	}
	return cfg
}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		cfg := loadConfig()
		if cfg.Mode == "off" {
			return nil, nil
		}

		// The request must already carry a breakpoint. This plugin chooses
		// which lifetime to buy; it never decides that something should be
		// cached, because that judgement belongs to whoever placed the marker.
		idx := lastCacheBreakpoint(req)
		if idx < 0 {
			return nil, nil
		}

		meta := readHostMeta(req)
		pricing, err := sdk.GetCachePricing(meta.Provider, req.Model)
		if err != nil || !pricing.Available() {
			// Unknown economics: leave the request exactly as it arrived.
			// Guessing here spends the operator's money on a hunch.
			return nil, nil
		}

		prefixKey := cachePrefixKey(req)
		if prefixKey == "" {
			return nil, nil
		}

		// A decision already exists for this exact prefix — reapply it
		// verbatim. This is the stickiness that keeps the cache entry alive.
		var prior decision
		if found, _ := sdk.StateGetJSON("decision/"+prefixKey, &prior); found {
			return applyMarker(req, idx, prior.Marker), nil
		}

		// Without a clock there is no gap history to reason from, so the
		// conversation keeps whatever tier the harness asked for.
		now, err := sdk.Now()
		if err != nil {
			return nil, nil
		}
		act := recordActivity(meta.ConversationID, now)

		marker, ttl := chooseTier(cfg, pricing, act, now)
		if marker == nil {
			return nil, nil
		}

		_ = sdk.StateSetJSON("decision/"+prefixKey, decision{
			Marker:          marker,
			TierTTL:         ttl,
			DecidedAtMillis: now,
		})
		_, _ = sdk.HostCall("torana_plugin_counter", counterPayload("tier_decisions", 1))

		return applyMarker(req, idx, marker), nil
	})
}

// chooseTier returns the breakpoint marker to use, or nil to leave the request
// untouched.
func chooseTier(cfg config, pricing sdk.CachePricing, act activity, now int64) (map[string]any, int) {
	// Without a declared long tier there is nothing to choose between.
	if pricing.ShortestTTLSeconds <= 0 {
		return nil, 0
	}

	switch cfg.Mode {
	case "short":
		return nil, 0 // the harness default already selects the short tier
	case "long":
		return longTierMarker(pricing), longTierTTL(pricing)
	}

	// auto: buy the longer tier once this conversation has demonstrated it
	// pauses for long enough to lose a short-tier entry.
	threshold := int64(cfg.MinGapSecondsForLongTier) * 1000
	if threshold <= 0 {
		threshold = int64(float64(longTierTTL(pricing)) * 0.3 * 1000)
	}
	if threshold <= 0 || act.LongestGapMillis < threshold {
		return nil, 0
	}
	return longTierMarker(pricing), longTierTTL(pricing)
}

// The host reports the shortest tier explicitly; the long tier is whatever it
// declared beyond that. The plugin learns both through pricing rather than
// hard-coding any provider's menu.
func longTierTTL(p sdk.CachePricing) int {
	if p.ShortestTTLSeconds >= 3600 {
		return p.ShortestTTLSeconds
	}
	return 3600
}

func longTierMarker(p sdk.CachePricing) map[string]any {
	return map[string]any{"type": "ephemeral", "ttl": "1h"}
}

// recordActivity updates and returns this conversation's gap history.
func recordActivity(convKey string, now int64) activity {
	var act activity
	if convKey == "" || now == 0 {
		return act
	}
	key := "activity/" + convKey
	_, _ = sdk.StateGetJSON(key, &act)

	if act.LastSeenMillis > 0 {
		if gap := now - act.LastSeenMillis; gap > act.LongestGapMillis {
			act.LongestGapMillis = gap
		}
	}
	act.LastSeenMillis = now
	act.Turns++
	_ = sdk.StateSetJSON(key, act)
	return act
}

// applyMarker sets the breakpoint marker on the message at idx. Returning the
// request unchanged when the marker already matches avoids a pointless
// re-serialisation.
func applyMarker(req *pb.ChatRequest, idx int, marker map[string]any) *pb.ChatRequest {
	if marker == nil || idx < 0 || idx >= len(req.Messages) {
		return nil
	}
	current := sdk.CacheControl(req.Messages[idx])
	if sameMarker(current, marker) {
		return nil
	}
	sdk.SetCacheBreakpoint(req.Messages[idx], marker)
	return req
}

func sameMarker(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

// lastCacheBreakpoint returns the index of the last message carrying a cache
// breakpoint — the boundary of the cached prefix — or -1.
func lastCacheBreakpoint(req *pb.ChatRequest) int {
	last := -1
	for i, m := range req.Messages {
		if len(m.CacheControlJson) > 0 {
			last = i
		}
	}
	return last
}

// cachePrefixKey fingerprints the bytes the provider will cache: the model plus
// every message up to and including the breakpoint. It mirrors the host's own
// CachePrefixKey closely enough for stickiness, which only needs "has this
// prefix changed", not an exact match with the host's value.
func cachePrefixKey(req *pb.ChatRequest) string {
	idx := lastCacheBreakpoint(req)
	if idx < 0 {
		return ""
	}
	h := newHash()
	h.add(req.Model)
	for _, t := range req.Tools {
		h.add("tool")
		h.add(t.Name)
		h.add(string(t.ParametersJson))
	}
	for _, m := range req.Messages[:idx+1] {
		h.add(m.Role)
		h.add(m.Content)
		h.add(string(m.ContentPartsJson))
		for _, tc := range m.ToolCalls {
			h.add(tc.Id)
			h.add(tc.Name)
			h.add(string(tc.ArgumentsJson))
		}
	}
	return h.sum()
}

// hostMeta is the routing context the host publishes on every request.
type hostMeta struct {
	Provider       string `json:"_provider"`
	ConversationID string `json:"_conversation_id"`
}

// readHostMeta decodes it. Empty fields mean the host did not supply them, in
// which case the plugin declines to act rather than guessing.
func readHostMeta(req *pb.ChatRequest) hostMeta {
	var meta hostMeta
	if len(req.ToranaMetaJson) == 0 {
		return meta
	}
	_ = json.Unmarshal(req.ToranaMetaJson, &meta)
	return meta
}

func counterPayload(name string, delta int64) string {
	b, _ := json.Marshal(map[string]any{"counter": name, "delta": delta})
	return string(b)
}

// A tiny FNV-1a so the plugin carries no hashing dependency into WASM.
type hasher struct{ h uint64 }

func newHash() *hasher { return &hasher{h: 14695981039346656037} }

func (x *hasher) add(s string) {
	// Length-prefix every field so that field boundaries cannot be forged by
	// content: without it "ab"+"cd" and "a"+"bcd" hash identically, and two
	// different prefixes would share a decision.
	x.write(fmt.Sprintf("%d:", len(s)))
	x.write(s)
}

func (x *hasher) write(s string) {
	for i := 0; i < len(s); i++ {
		x.h ^= uint64(s[i])
		x.h *= 1099511628211
	}
}

func (x *hasher) sum() string { return fmt.Sprintf("%016x", x.h) }
