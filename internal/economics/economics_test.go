package economics

import "testing"

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

func close(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-12
}
