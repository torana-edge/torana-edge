package proxy

import (
	"context"
	"encoding/json"
	"math"
	"sort"

	"github.com/torana-edge/torana-edge/internal/wasm"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// cachePricingRequest is what a plugin asks about.
type cachePricingRequest struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// cachePricingResponse is data, never a decision.
//
// The host holds prices and cache semantics because they are operator
// configuration; the plugin holds policy because that is what a plugin is for.
// So this reports what a refresh costs and how many are affordable, and says
// nothing about whether to send one.
type cachePricingResponse struct {
	Status string `json:"status"` // "ok" | "unavailable"
	Reason string `json:"reason,omitempty"`

	CacheReadUSDPerMTok  float64 `json:"cache_read_usd_per_mtok,omitempty"`
	CacheWriteUSDPerMTok float64 `json:"cache_write_usd_per_mtok,omitempty"`

	// WriteReadRatio is cache-write price over cache-read price. Refreshing a
	// cached prefix is worth it only while fewer than (ratio - 1) refreshes
	// have been spent, because letting the entry lapse costs one write and
	// holding it open costs one read per refresh. The prefix size cancels out
	// of that comparison entirely, which is why this is a bare ratio and not
	// something that needs a token count.
	WriteReadRatio float64 `json:"write_read_ratio,omitempty"`

	// BreakEvenRefreshes is floor(ratio - 1): the largest number of refreshes
	// that still costs less than accepting the cache miss.
	BreakEvenRefreshes int `json:"break_even_refreshes,omitempty"`

	// RefreshOnRead reports whether reads restart the entry's clock. When
	// false, no amount of refreshing keeps anything alive and the affordability
	// numbers above are irrelevant.
	RefreshOnRead bool `json:"refresh_on_read"`

	// ShortestTTLSeconds and WarmIntervalSeconds describe the tier a refresh
	// loop races against. Zero when the provider declares no tiers.
	ShortestTTLSeconds  int `json:"shortest_ttl_seconds,omitempty"`
	WarmIntervalSeconds int `json:"warm_interval_seconds,omitempty"`

	// Tiers are the cache lifetimes this provider sells, ascending by TTL,
	// each with the opaque breakpoint marker that selects it.
	//
	// Without these a plugin choosing between lifetimes has to hard-code one
	// provider's menu, which is what cache_tier_selector did — writing
	// Anthropic's marker into every provider's requests and silently ignoring
	// whatever the operator had configured.
	Tiers []cachePricingTier `json:"tiers,omitempty"`
}

// cachePricingTier is one purchasable cache lifetime, as the operator declared it.
type cachePricingTier struct {
	TTLSeconds int `json:"ttl_seconds"`
	// WriteMultiplier is the cost of writing this tier relative to the model's
	// base input rate, so a plugin can compare tiers without knowing the model.
	WriteMultiplier float64 `json:"write_multiplier,omitempty"`
	// Marker is placed verbatim into the request's cache breakpoint. Torana
	// never interprets it.
	Marker map[string]any `json:"marker,omitempty"`
}

// Reasons a plugin may see for a domain "unavailable" value. Named so the
// plugin can branch on them rather than matching prose. Caller bugs (malformed
// payload) and operator gaps (unknown provider) are FRAMED refusals instead —
// see cachePricing.
const (
	pricingReasonNoPricing     = "no_pricing_configured"
	pricingReasonNoCacheRates  = "no_cache_rates_configured"
	pricingReasonNoCacheConfig = "no_cache_semantics_configured"
)

// cachePricing answers the torana_cache_pricing host call.
//
// The classification split matters here: a caller bug and an operator gap are
// different domains, and a plugin must be able to tell them apart. Malformed
// JSON and a missing required provider/model are INVALID_ARGUMENT — retrying
// the same call cannot help. Naming a provider that is not configured is
// NOT_CONFIGURED — the operator must add it. Everything else is a legitimate
// QUERY RESULT and stays a domain value: an unpriced model, a provider with
// no cache rates, or one with no cache semantics is "unavailable" as DATA
// (the plugin must decline to spend), not a transport failure of the host
// call. Priced is "ok". A plugin that cannot price its action must decline to
// spend money, and it can only do that if the host refuses to invent numbers.
func (s *Server) cachePricing(_ context.Context, payloadJSON string) wasm.ExtensionResult {
	var req cachePricingRequest
	if err := json.Unmarshal([]byte(payloadJSON), &req); err != nil {
		return wasm.ExtensionRefusal(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT,
			"invalid payload: %v", err)
	}
	if req.Provider == "" {
		return wasm.ExtensionRefusal(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "provider is required")
	}
	if req.Model == "" {
		return wasm.ExtensionRefusal(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "model is required")
	}

	cfg := s.GetConfig().Providers
	prov, ok := cfg.Providers[req.Provider]
	if !ok {
		return wasm.ExtensionRefusal(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED,
			"unknown provider %q", req.Provider)
	}

	out := cachePricingResponse{Status: "ok"}

	if prov.Cache != nil {
		out.RefreshOnRead = prov.Cache.RefreshOnRead
		if shortest, ok := prov.Cache.ShortestTier(); ok {
			out.ShortestTTLSeconds = shortest.TTLSeconds
		}
		for _, t := range prov.Cache.Tiers {
			out.Tiers = append(out.Tiers, cachePricingTier{
				TTLSeconds:      t.TTLSeconds,
				WriteMultiplier: t.WriteMultiplier,
				Marker:          t.Marker,
			})
		}
		sort.Slice(out.Tiers, func(i, j int) bool {
			return out.Tiers[i].TTLSeconds < out.Tiers[j].TTLSeconds
		})
		out.WarmIntervalSeconds = int(prov.Cache.WarmInterval().Seconds())
	}

	pricing, priced := prov.PricingFor(req.Model)
	if !priced {
		out.Status = "unavailable"
		out.Reason = pricingReasonNoPricing
		return wasm.ExtensionValue([]byte(marshalPricing(out)))
	}
	if pricing.CacheReadUSDPerMTok == nil || pricing.CacheWriteUSDPerMTok == nil {
		out.Status = "unavailable"
		out.Reason = pricingReasonNoCacheRates
		return wasm.ExtensionValue([]byte(marshalPricing(out)))
	}

	read, write := *pricing.CacheReadUSDPerMTok, *pricing.CacheWriteUSDPerMTok
	out.CacheReadUSDPerMTok = read
	out.CacheWriteUSDPerMTok = write

	// A zero or negative read rate makes the ratio meaningless (and would
	// divide by zero). Report the rates but no affordability, so a plugin
	// cannot read "infinite refreshes are free" out of a free cache.
	if read > 0 && write >= 0 {
		out.WriteReadRatio = write / read
		if n := math.Floor(out.WriteReadRatio - 1); n > 0 {
			out.BreakEvenRefreshes = int(n)
		}
	}

	if prov.Cache == nil {
		// Priced, but with no declared lifetime there is nothing to refresh
		// against. Say so rather than let a plugin assume a default TTL.
		out.Status = "unavailable"
		out.Reason = pricingReasonNoCacheConfig
	}
	return wasm.ExtensionValue([]byte(marshalPricing(out)))
}

func marshalPricing(r cachePricingResponse) string {
	b, err := json.Marshal(r)
	if err != nil {
		return `{"status":"unavailable","reason":"encode_failed"}`
	}
	return string(b)
}
