// Package economics contains provider-neutral usage and compaction cost models.
// It deliberately contains no built-in prices: rates change frequently and an
// operator must opt in by configuring the provider/model they actually use.
package economics

import "math"

const (
	UnavailablePricing              = "pricing_unconfigured"
	UnavailableTokenEstimate        = "token_estimate_unavailable"
	UnavailableExpectedApplications = "expected_applications_unavailable"
	UnavailableOffloadUsage         = "offload_usage_unavailable"
	UnavailableNonPositiveNet       = "non_positive_estimated_net"
	UnavailableRouteUnresolved      = "route_unresolved"
	UnavailableFallbackUnpriced     = "fallback_unpriced_or_non_positive"
)

// ModelPricing is an operator-supplied price sheet, in USD per million tokens.
// Pointers distinguish an explicitly free rate (0) from an unknown rate.
type ModelPricing struct {
	InputUSDPerMTok      *float64 `json:"input_usd_per_mtok,omitempty"`
	OutputUSDPerMTok     *float64 `json:"output_usd_per_mtok,omitempty"`
	CacheReadUSDPerMTok  *float64 `json:"cache_read_usd_per_mtok,omitempty"`
	CacheWriteUSDPerMTok *float64 `json:"cache_write_usd_per_mtok,omitempty"`
}

// Usage is billable token usage returned by an upstream. InputTokens is the
// provider's reported input count; InputIncludesCacheRead records whether
// CacheReadTokens is a subset of it (OpenAI-style) or additional
// (Anthropic-style). This avoids silently double-charging cache reads.
type Usage struct {
	Reported               bool  `json:"reported"`
	InputTokens            int64 `json:"input_tokens,omitempty"`
	OutputTokens           int64 `json:"output_tokens,omitempty"`
	CacheReadTokens        int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens       int64 `json:"cache_write_tokens,omitempty"`
	InputIncludesCacheRead bool  `json:"input_includes_cache_read,omitempty"`
}

// Cost returns the configured cost of usage. ok is false if a rate required
// by a non-zero usage bucket is absent.
func (u Usage) Cost(p ModelPricing) (cost float64, ok bool) {
	input := u.InputTokens
	if u.InputIncludesCacheRead {
		input -= u.CacheReadTokens
		if input < 0 {
			return 0, false
		}
	}
	parts := []struct {
		tokens int64
		rate   *float64
	}{
		{input, p.InputUSDPerMTok},
		{u.OutputTokens, p.OutputUSDPerMTok},
		{u.CacheReadTokens, p.CacheReadUSDPerMTok},
		{u.CacheWriteTokens, p.CacheWriteUSDPerMTok},
	}
	for _, part := range parts {
		if part.tokens <= 0 {
			continue
		}
		if part.rate == nil || *part.rate < 0 {
			return 0, false
		}
		cost += float64(part.tokens) * *part.rate / 1_000_000
	}
	return cost, true
}

// Offload records the summarizer call that produced a compaction.
type Offload struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Usage    Usage  `json:"usage,omitempty"`
}

// OffloadResult is returned by the richer offload host callback. Extra JSON
// fields are backward-compatible with plugins that only decode completion.
type OffloadResult struct {
	Completion string `json:"completion"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Usage      Usage  `json:"usage"`
}

// CompactionReport is the backward-compatible superset accepted by
// torana_record_savings. A batch must be reported once, with RewriteSpanTokens
// measured from the earliest changed item to the end of the new prompt.
type CompactionReport struct {
	OriginalBytes int64 `json:"original_bytes"`
	FinalBytes    int64 `json:"final_bytes"`

	EstimatedTokensRemoved     int64  `json:"estimated_tokens_removed,omitempty"`
	EstimatedRewriteSpanTokens int64  `json:"estimated_rewrite_span_tokens,omitempty"`
	Estimator                  string `json:"estimator,omitempty"`
	CandidateCount             int64  `json:"candidate_count,omitempty"`
	ExpectedApplications       int64  `json:"expected_applications,omitempty"`

	// Source distinguishes a newly generated canonical replacement from an
	// application of a cached replacement. Empty denotes the legacy ABI.
	Source  string   `json:"source,omitempty"` // transformation, cache_reuse, legacy
	Offload *Offload `json:"offload,omitempty"`

	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

func (r *CompactionReport) Normalize() {
	if r.CandidateCount == 0 {
		r.CandidateCount = 1
	}
	if r.Source == "" {
		r.Source = "legacy"
	}
}

func (r CompactionReport) Valid() bool {
	return r.OriginalBytes >= 0 && r.FinalBytes >= 0 &&
		r.EstimatedTokensRemoved >= 0 && r.EstimatedRewriteSpanTokens >= 0 &&
		r.CandidateCount > 0 && r.ExpectedApplications >= 0 &&
		(r.Source == "legacy" || r.Source == "transformation" || r.Source == "cache_reuse")
}

// SavingsEstimate is intentionally explicit when dollars cannot be computed.
// EstimatedGrossUSD is the repeated cached-input reduction before rewrite and
// offload costs. EstimatedNetUSD subtracts those costs.
type SavingsEstimate struct {
	EstimatedGrossUSD *float64 `json:"estimated_gross_usd,omitempty"`
	EstimatedNetUSD   *float64 `json:"estimated_net_usd,omitempty"`
	UnavailableReason string   `json:"unavailable_reason,omitempty"`
}

type CompactionDecision struct {
	Apply             bool     `json:"apply"`
	Reason            string   `json:"reason,omitempty"`
	EstimatedGrossUSD *float64 `json:"estimated_gross_usd,omitempty"`
	EstimatedNetUSD   *float64 `json:"estimated_net_usd,omitempty"`
}

func DecideCompaction(r CompactionReport, target *ModelPricing, offloadPricing *ModelPricing) CompactionDecision {
	if target == nil {
		return CompactionDecision{Reason: UnavailablePricing}
	}
	est := EstimateSavings(r, *target, offloadPricing)
	decision := CompactionDecision{
		Reason:            est.UnavailableReason,
		EstimatedGrossUSD: est.EstimatedGrossUSD,
		EstimatedNetUSD:   est.EstimatedNetUSD,
	}
	if est.EstimatedNetUSD != nil && *est.EstimatedNetUSD > 0 {
		decision.Apply = true
		decision.Reason = "estimated_net_positive"
	} else if est.EstimatedNetUSD != nil {
		decision.Reason = UnavailableNonPositiveNet
	}
	return decision
}

// EstimateSavings applies the conservative cached-prefix formula. N includes
// the first request carrying the rewritten prefix. Cache-write cost is charged
// once for the whole batch rewrite span, never once per candidate.
func EstimateSavings(r CompactionReport, target ModelPricing, offloadPricing *ModelPricing) SavingsEstimate {
	if r.EstimatedTokensRemoved <= 0 || r.EstimatedRewriteSpanTokens <= 0 {
		return SavingsEstimate{UnavailableReason: UnavailableTokenEstimate}
	}
	if r.ExpectedApplications <= 0 {
		return SavingsEstimate{UnavailableReason: UnavailableExpectedApplications}
	}
	if target.CacheReadUSDPerMTok == nil || target.CacheWriteUSDPerMTok == nil {
		return SavingsEstimate{UnavailableReason: UnavailablePricing}
	}
	readRate, writeRate := *target.CacheReadUSDPerMTok, *target.CacheWriteUSDPerMTok
	if readRate < 0 || writeRate < 0 {
		return SavingsEstimate{UnavailableReason: UnavailablePricing}
	}
	gross := float64(r.ExpectedApplications*r.EstimatedTokensRemoved) * readRate / 1_000_000
	rewritePremium := 0.0
	// A cached replacement has already established the compact prefix. Reusing
	// the same bytes on another request does not cause another cache rewrite.
	if r.Source != "cache_reuse" {
		rewritePremium = float64(r.EstimatedRewriteSpanTokens) * math.Max(0, writeRate-readRate) / 1_000_000
	}
	offloadCost := 0.0
	if r.Offload != nil {
		if !r.Offload.Usage.Reported {
			return SavingsEstimate{EstimatedGrossUSD: &gross, UnavailableReason: UnavailableOffloadUsage}
		}
		if offloadPricing == nil {
			return SavingsEstimate{EstimatedGrossUSD: &gross, UnavailableReason: UnavailablePricing}
		}
		var ok bool
		offloadCost, ok = r.Offload.Usage.Cost(*offloadPricing)
		if !ok {
			return SavingsEstimate{EstimatedGrossUSD: &gross, UnavailableReason: UnavailableOffloadUsage}
		}
	}
	net := gross - rewritePremium - offloadCost
	return SavingsEstimate{EstimatedGrossUSD: &gross, EstimatedNetUSD: &net}
}

// EstimateApplicationSavings estimates the single request represented by a
// recorded application. Unlike the decision projection, it never multiplies
// by ExpectedApplications, so repeated cache-reuse reports cannot double-count
// projected future savings. Rewrite and offload costs are charged only on a
// transformation report.
func EstimateApplicationSavings(r CompactionReport, target ModelPricing, offloadPricing *ModelPricing) SavingsEstimate {
	if r.EstimatedTokensRemoved <= 0 {
		return SavingsEstimate{UnavailableReason: UnavailableTokenEstimate}
	}
	if target.CacheReadUSDPerMTok == nil || *target.CacheReadUSDPerMTok < 0 {
		return SavingsEstimate{UnavailableReason: UnavailablePricing}
	}
	gross := float64(r.EstimatedTokensRemoved) * *target.CacheReadUSDPerMTok / 1_000_000
	if r.Source == "cache_reuse" {
		net := gross
		return SavingsEstimate{EstimatedGrossUSD: &gross, EstimatedNetUSD: &net}
	}
	if r.EstimatedRewriteSpanTokens <= 0 || target.CacheWriteUSDPerMTok == nil || *target.CacheWriteUSDPerMTok < 0 {
		return SavingsEstimate{EstimatedGrossUSD: &gross, UnavailableReason: UnavailablePricing}
	}
	net := gross - float64(r.EstimatedRewriteSpanTokens)*math.Max(0, *target.CacheWriteUSDPerMTok-*target.CacheReadUSDPerMTok)/1_000_000
	if r.Offload != nil {
		if !r.Offload.Usage.Reported {
			return SavingsEstimate{EstimatedGrossUSD: &gross, UnavailableReason: UnavailableOffloadUsage}
		}
		if offloadPricing == nil {
			return SavingsEstimate{EstimatedGrossUSD: &gross, UnavailableReason: UnavailablePricing}
		}
		cost, ok := r.Offload.Usage.Cost(*offloadPricing)
		if !ok {
			return SavingsEstimate{EstimatedGrossUSD: &gross, UnavailableReason: UnavailableOffloadUsage}
		}
		net -= cost
	}
	return SavingsEstimate{EstimatedGrossUSD: &gross, EstimatedNetUSD: &net}
}
