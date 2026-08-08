package economics

import (
	"math"
	"testing"
)

func rate(v float64) *float64 { return &v }

func TestEstimateSavingsChargesBatchRewriteOnce(t *testing.T) {
	pricing := ModelPricing{CacheReadUSDPerMTok: rate(0.5), CacheWriteUSDPerMTok: rate(6.25)}
	report := CompactionReport{
		EstimatedTokensRemoved:     95_000,
		EstimatedRewriteSpanTokens: 15_000,
		ExpectedApplications:       8,
		CandidateCount:             4,
		Source:                     "transformation",
	}
	est := EstimateSavings(report, pricing, nil)
	if est.UnavailableReason != "" || est.EstimatedGrossUSD == nil || est.EstimatedNetUSD == nil {
		t.Fatalf("estimate unavailable: %+v", est)
	}
	if got, want := *est.EstimatedGrossUSD, 0.38; !close(got, want) {
		t.Fatalf("gross=%f want %f", got, want)
	}
	// 0.38 - one 15k-token rewrite premium at (6.25 - 0.5)/MTok.
	if got, want := *est.EstimatedNetUSD, 0.29375; !close(got, want) {
		t.Fatalf("net=%f want %f", got, want)
	}
}

func TestDecideCompactionRequiresPricingAndPositiveNet(t *testing.T) {
	report := CompactionReport{EstimatedTokensRemoved: 100, EstimatedRewriteSpanTokens: 10_000, ExpectedApplications: 1, Source: "transformation", CandidateCount: 1}
	if got := DecideCompaction(report, nil, nil); got.Apply || got.Reason != UnavailablePricing {
		t.Fatalf("missing pricing decision=%+v", got)
	}
	pricing := ModelPricing{CacheReadUSDPerMTok: rate(0.1), CacheWriteUSDPerMTok: rate(1)}
	if got := DecideCompaction(report, &pricing, nil); got.Apply || got.Reason != UnavailableNonPositiveNet {
		t.Fatalf("losing decision=%+v", got)
	}
	report.ExpectedApplications = 1000
	if got := DecideCompaction(report, &pricing, nil); !got.Apply || got.Reason != "estimated_net_positive" {
		t.Fatalf("winning decision=%+v", got)
	}
}

func TestUsageCostDoesNotDoubleChargeCacheReads(t *testing.T) {
	p := ModelPricing{InputUSDPerMTok: rate(2), OutputUSDPerMTok: rate(4), CacheReadUSDPerMTok: rate(0.2)}
	u := Usage{Reported: true, InputTokens: 1_000, OutputTokens: 100, CacheReadTokens: 800, InputIncludesCacheRead: true}
	got, ok := u.Cost(p)
	if !ok {
		t.Fatal("cost unexpectedly unavailable")
	}
	// 200 uncached * $2/M + 800 cached * $0.2/M + 100 output * $4/M.
	if want := 0.00096; !close(got, want) {
		t.Fatalf("cost=%f want %f", got, want)
	}
}

func TestApplicationSavingsDoesNotProjectCacheReuse(t *testing.T) {
	p := ModelPricing{CacheReadUSDPerMTok: rate(0.5), CacheWriteUSDPerMTok: rate(1)}
	r := CompactionReport{EstimatedTokensRemoved: 10_000, ExpectedApplications: 100, Source: "cache_reuse"}
	est := EstimateApplicationSavings(r, p, nil)
	if est.EstimatedNetUSD == nil || !close(*est.EstimatedNetUSD, 0.005) {
		t.Fatalf("cache reuse must count one realized application, got %+v", est)
	}
}

func TestDecisionDoesNotRechargeRewriteForCachedReplacement(t *testing.T) {
	p := ModelPricing{CacheReadUSDPerMTok: rate(0.5), CacheWriteUSDPerMTok: rate(10)}
	r := CompactionReport{
		EstimatedTokensRemoved:     1_000,
		EstimatedRewriteSpanTokens: 100_000,
		ExpectedApplications:       1,
		Source:                     "cache_reuse",
	}
	decision := DecideCompaction(r, &p, nil)
	if !decision.Apply {
		t.Fatalf("stable cached replacement should not pay another rewrite: %+v", decision)
	}
}

func TestUsageCostRejectsInvalidInputsAndNonFiniteResults(t *testing.T) {
	tests := []struct {
		name    string
		usage   Usage
		pricing ModelPricing
	}{
		{"negative input", Usage{InputTokens: -1}, ModelPricing{}},
		{"negative output", Usage{OutputTokens: -1}, ModelPricing{}},
		{"negative cache read", Usage{CacheReadTokens: -1}, ModelPricing{}},
		{"negative cache write", Usage{CacheWriteTokens: -1}, ModelPricing{}},
		{"cache read exceeds inclusive input", Usage{InputTokens: 1, CacheReadTokens: 2, InputIncludesCacheRead: true}, ModelPricing{}},
		{"nan rate", Usage{InputTokens: 1}, ModelPricing{InputUSDPerMTok: rate(math.NaN())}},
		{"positive infinity rate", Usage{InputTokens: 1}, ModelPricing{InputUSDPerMTok: rate(math.Inf(1))}},
		{"finite rate overflows dollars", Usage{InputTokens: math.MaxInt64}, ModelPricing{InputUSDPerMTok: rate(math.MaxFloat64)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := tc.usage.Cost(tc.pricing); ok || got != 0 {
				t.Fatalf("Cost() = (%v, %v), want (0, false)", got, ok)
			}
		})
	}
}

func TestEstimateSavingsDoesNotOverflowIntegerProduct(t *testing.T) {
	p := ModelPricing{CacheReadUSDPerMTok: rate(1), CacheWriteUSDPerMTok: rate(1)}
	r := CompactionReport{
		EstimatedTokensRemoved:     math.MaxInt64,
		EstimatedRewriteSpanTokens: 1,
		ExpectedApplications:       2,
		Source:                     "cache_reuse",
	}
	est := EstimateSavings(r, p, nil)
	if est.UnavailableReason != "" || est.EstimatedGrossUSD == nil || est.EstimatedNetUSD == nil {
		t.Fatalf("large finite estimate unavailable: %+v", est)
	}
	want := float64(math.MaxInt64) * 2 / 1_000_000
	if *est.EstimatedGrossUSD != want || *est.EstimatedNetUSD != want {
		t.Fatalf("estimate = %+v, want gross/net %v", est, want)
	}
}

func TestSavingsEstimatorsRejectNonFiniteRatesAndResults(t *testing.T) {
	base := CompactionReport{
		EstimatedTokensRemoved:     1,
		EstimatedRewriteSpanTokens: 1,
		ExpectedApplications:       1,
		Source:                     "transformation",
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		p := ModelPricing{CacheReadUSDPerMTok: rate(bad), CacheWriteUSDPerMTok: rate(1)}
		if got := EstimateSavings(base, p, nil); got.UnavailableReason != UnavailablePricing || got.EstimatedGrossUSD != nil || got.EstimatedNetUSD != nil {
			t.Fatalf("EstimateSavings(%v) = %+v", bad, got)
		}
		if got := EstimateApplicationSavings(base, p, nil); got.UnavailableReason != UnavailablePricing || got.EstimatedGrossUSD != nil || got.EstimatedNetUSD != nil {
			t.Fatalf("EstimateApplicationSavings(%v) = %+v", bad, got)
		}
	}
	overflow := ModelPricing{CacheReadUSDPerMTok: rate(math.MaxFloat64), CacheWriteUSDPerMTok: rate(math.MaxFloat64)}
	overflowReport := base
	overflowReport.EstimatedTokensRemoved = math.MaxInt64
	if got := EstimateSavings(overflowReport, overflow, nil); got.UnavailableReason != UnavailableNonFiniteEstimate || got.EstimatedGrossUSD != nil || got.EstimatedNetUSD != nil {
		t.Fatalf("overflowing projected estimate escaped: %+v", got)
	}
	if got := EstimateApplicationSavings(overflowReport, overflow, nil); got.UnavailableReason != UnavailableNonFiniteEstimate || got.EstimatedGrossUSD != nil || got.EstimatedNetUSD != nil {
		t.Fatalf("overflowing application estimate escaped: %+v", got)
	}
}

func close(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-12
}
