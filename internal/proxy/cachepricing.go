package proxy

import (
	"context"
	"encoding/json"
	"math"
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
}

// Reasons a plugin may see. Named so the plugin can branch on them rather than
// matching prose.
const (
	pricingReasonUnknownProvider = "unknown_provider"
	pricingReasonNoPricing       = "no_pricing_configured"
	pricingReasonNoCacheRates    = "no_cache_rates_configured"
	pricingReasonNoCacheConfig   = "no_cache_semantics_configured"
	pricingReasonBadPayload      = "invalid_payload"
)

// cachePricing answers the torana_cache_pricing host call.
//
// Every failure is an explicit "unavailable" with a machine-readable reason
// rather than a guess. That is the same discipline the compaction gate follows:
// a plugin that cannot price its action must decline to spend money, and it can
// only do that if the host refuses to invent numbers.
func (s *Server) cachePricing(_ context.Context, payloadJSON string) string {
	var req cachePricingRequest
	if err := json.Unmarshal([]byte(payloadJSON), &req); err != nil {
		return marshalPricing(cachePricingResponse{Status: "unavailable", Reason: pricingReasonBadPayload})
	}

	cfg := s.GetConfig().Providers
	prov, ok := cfg.Providers[req.Provider]
	if !ok {
		return marshalPricing(cachePricingResponse{Status: "unavailable", Reason: pricingReasonUnknownProvider})
	}

	out := cachePricingResponse{Status: "ok"}

	if prov.Cache != nil {
		out.RefreshOnRead = prov.Cache.RefreshOnRead
		if shortest, ok := prov.Cache.ShortestTier(); ok {
			out.ShortestTTLSeconds = shortest.TTLSeconds
		}
		out.WarmIntervalSeconds = int(prov.Cache.WarmInterval().Seconds())
	}

	pricing, priced := prov.PricingFor(req.Model)
	if !priced {
		out.Status = "unavailable"
		out.Reason = pricingReasonNoPricing
		return marshalPricing(out)
	}
	if pricing.CacheReadUSDPerMTok == nil || pricing.CacheWriteUSDPerMTok == nil {
		out.Status = "unavailable"
		out.Reason = pricingReasonNoCacheRates
		return marshalPricing(out)
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
	return marshalPricing(out)
}

func marshalPricing(r cachePricingResponse) string {
	b, err := json.Marshal(r)
	if err != nil {
		return `{"status":"unavailable","reason":"encode_failed"}`
	}
	return string(b)
}
