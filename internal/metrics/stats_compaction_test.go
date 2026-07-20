package metrics

import (
	"testing"

	"github.com/torana-edge/torana-edge/internal/economics"
)

func price(v float64) *float64 { return &v }

func TestRecordCompactionReportSeparatesLifecycleCounters(t *testing.T) {
	s := NewStatsTracker()
	p := economics.ModelPricing{CacheReadUSDPerMTok: price(0.5), CacheWriteUSDPerMTok: price(1)}
	base := economics.CompactionReport{
		OriginalBytes:              40_000,
		FinalBytes:                 4_000,
		EstimatedTokensRemoved:     9_000,
		EstimatedRewriteSpanTokens: 2_000,
		Estimator:                  "bytes/4-v1",
		CandidateCount:             3,
		ExpectedApplications:       5,
		Source:                     "transformation",
	}
	s.RecordCompactionReport("compactor", base, &p, nil)
	reuse := base
	reuse.Source = "cache_reuse"
	reuse.ExpectedApplications = 0 // a replay is counted, not re-projected
	s.RecordCompactionReport("compactor", reuse, &p, nil)

	got := s.Snapshot()
	if got.CompactionApplications != 2 || got.CompactionTransformations != 1 || got.CompactionCacheReuses != 1 {
		t.Fatalf("lifecycle counters wrong: %+v", got)
	}
	if got.EstimatedTokensAvoided != 18_000 {
		t.Fatalf("estimated avoided=%d want 18000", got.EstimatedTokensAvoided)
	}
	if got.EstimatedRewriteSpanTokens != 2_000 {
		t.Fatalf("rewrite span charged more than once: %d", got.EstimatedRewriteSpanTokens)
	}
	if got.PerPlugin["compactor"].Transformations != 1 || got.PerPlugin["compactor"].CacheReuses != 1 {
		t.Fatalf("per-plugin lifecycle wrong: %+v", got.PerPlugin["compactor"])
	}
	if got.EstimatedGrossUSD == nil || got.EstimatedNetUSD == nil {
		t.Fatalf("priced applications must expose dollar totals: %+v", got)
	}
}

func TestRecordCompactionReportExplainsUnavailableDollars(t *testing.T) {
	s := NewStatsTracker()
	s.RecordCompactionReport("compactor", economics.CompactionReport{
		OriginalBytes: 100, FinalBytes: 10, EstimatedTokensRemoved: 20,
		EstimatedRewriteSpanTokens: 5, ExpectedApplications: 2, Source: "transformation",
	}, nil, nil)
	snapshot := s.Snapshot()
	if got := snapshot.SavingsUnavailable[economics.UnavailablePricing]; got != 1 {
		t.Fatalf("pricing unavailable count=%d want 1", got)
	}
	if snapshot.EstimatedGrossUSD != nil || snapshot.EstimatedNetUSD != nil {
		t.Fatalf("unavailable dollar totals must be omitted: %+v", snapshot)
	}
}

func TestRecordOffloadUsage(t *testing.T) {
	s := NewStatsTracker()
	s.RecordOffloadUsage(economics.Usage{Reported: true, InputTokens: 1200, OutputTokens: 80, CacheReadTokens: 900, CacheWriteTokens: 100})
	s.RecordOffloadUsage(economics.Usage{})
	got := s.Snapshot()
	if got.OffloadInputTokens != 1200 || got.OffloadOutputTokens != 80 || got.OffloadCacheReadTokens != 900 || got.OffloadCacheWriteTokens != 100 {
		t.Fatalf("offload totals wrong: %+v", got)
	}
}
