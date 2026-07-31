package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/economics"
	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

type economicsRoundTripFunc func(*http.Request) (*http.Response, error)

func (f economicsRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func usd(v float64) *float64 { return &v }

func TestEvaluateCompactionUsesRequestRoutePricing(t *testing.T) {
	s := &Server{config: Config{Providers: provider.Config{Providers: map[string]provider.Provider{
		"openai": {Pricing: map[string]economics.ModelPricing{
			"gpt-test": {CacheReadUSDPerMTok: usd(0.5), CacheWriteUSDPerMTok: usd(1)},
		}},
	}}}}
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{Provider: "openai", Model: "gpt-test"})
	report := economics.CompactionReport{
		OriginalBytes: 100_000, FinalBytes: 5_000,
		EstimatedTokensRemoved: 95_000, EstimatedRewriteSpanTokens: 15_000,
		ExpectedApplications: 8, CandidateCount: 3, Source: "transformation",
	}
	if got := s.evaluateCompaction(ctx, report); !got.Apply || got.EstimatedNetUSD == nil {
		t.Fatalf("expected profitable decision, got %+v", got)
	}
}

func TestCompactionReportsCommitOnlyForUpstreamRequest(t *testing.T) {
	s := &Server{stats: metrics.NewStatsTracker()}
	report := attributedCompactionReport{Plugin: "compactor", Report: economics.CompactionReport{
		OriginalBytes: 100, FinalBytes: 10, Source: "legacy",
	}}
	rs := &reqState{CompactionReports: []attributedCompactionReport{report}}
	s.recordCompactionReports(rs)
	if got := s.stats.Snapshot().CompactionApplications; got != 0 {
		t.Fatalf("uncommitted report recorded %d applications", got)
	}
	rs.CompactionReportsCommitted = true
	s.recordCompactionReports(rs)
	if got := s.stats.Snapshot().CompactionApplications; got != 1 {
		t.Fatalf("committed report recorded %d applications, want 1", got)
	}
}

func TestUpstreamRoundTripCommitsPreparedReports(t *testing.T) {
	rl := NewRateLimiter(0, 0)
	defer rl.Close()
	rs := &reqState{CompactionRequestPrepared: true}
	ctx := context.WithValue(context.Background(), reqStateKey{}, rs)
	ctx = context.WithValue(ctx, routeContextKey{}, &RouteContext{ProviderName: "p"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://example.test/v1", strings.NewReader("{}"))
	transport := &failoverRoundTripper{
		base: economicsRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
		}),
		cfg:         func() provider.Config { return provider.Config{Providers: map[string]provider.Provider{"p": {}}} },
		rateLimiter: rl,
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()
	if !rs.CompactionReportsCommitted {
		t.Fatal("prepared report was not committed at upstream RoundTrip")
	}
}

func TestEvaluateCompactionFailsClosedWithoutPricing(t *testing.T) {
	s := &Server{config: Config{Providers: provider.Config{Providers: map[string]provider.Provider{"openai": {}}}}}
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{Provider: "openai", Model: "gpt-test"})
	report := economics.CompactionReport{
		OriginalBytes: 10, FinalBytes: 1, EstimatedTokensRemoved: 2,
		EstimatedRewriteSpanTokens: 1, ExpectedApplications: 10, CandidateCount: 1, Source: "transformation",
	}
	if got := s.evaluateCompaction(ctx, report); got.Apply || got.Reason != economics.UnavailablePricing {
		t.Fatalf("unpriced compaction must not apply: %+v", got)
	}
}

func TestEvaluateCompactionUsesEarlierRoutingVerdict(t *testing.T) {
	s := &Server{config: Config{Providers: provider.Config{Providers: map[string]provider.Provider{
		"initial": {Format: "openai"},
		"routed": {Format: "openai", Pricing: map[string]economics.ModelPricing{
			"cheap": {CacheReadUSDPerMTok: usd(0.5), CacheWriteUSDPerMTok: usd(1)},
		}},
	}}}}
	rs := &reqState{
		Provider: "initial", Model: "expensive", InitialProvider: "initial", InitialFormat: "openai",
		PendingRoute: &wasm.RouteVerdict{Provider: "routed", Model: "cheap"},
	}
	ctx := context.WithValue(context.Background(), reqStateKey{}, rs)
	report := economics.CompactionReport{
		OriginalBytes: 100_000, FinalBytes: 5_000, EstimatedTokensRemoved: 95_000,
		EstimatedRewriteSpanTokens: 15_000, ExpectedApplications: 8, CandidateCount: 1, Source: "transformation",
	}
	if got := s.evaluateCompaction(ctx, report); !got.Apply {
		t.Fatalf("expected routed pricing to apply, got %+v", got)
	}
	rs.PendingRoute.Provider = "missing"
	if got := s.evaluateCompaction(ctx, report); got.Apply || got.Reason != economics.UnavailableRouteUnresolved {
		t.Fatalf("unresolved reroute must fail closed: %+v", got)
	}
}

func TestEvaluateCompactionRequiresEveryFallbackToBeEconomic(t *testing.T) {
	winning := economics.ModelPricing{CacheReadUSDPerMTok: usd(0.5), CacheWriteUSDPerMTok: usd(1)}
	s := &Server{config: Config{Providers: provider.Config{Providers: map[string]provider.Provider{
		"primary":  {Format: "openai", Fallback: []string{"fallback"}, Pricing: map[string]economics.ModelPricing{"m": winning}},
		"fallback": {Format: "openai"},
	}}}}
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{Provider: "primary", Model: "m"})
	report := economics.CompactionReport{
		OriginalBytes: 100_000, FinalBytes: 5_000, EstimatedTokensRemoved: 95_000,
		EstimatedRewriteSpanTokens: 15_000, ExpectedApplications: 8, CandidateCount: 1, Source: "transformation",
	}
	if got := s.evaluateCompaction(ctx, report); got.Apply || got.Reason != economics.UnavailableFallbackUnpriced {
		t.Fatalf("unpriced fallback must fail closed: %+v", got)
	}
	fallback := s.config.Providers.Providers["fallback"]
	fallback.Pricing = map[string]economics.ModelPricing{"m": winning}
	s.config.Providers.Providers["fallback"] = fallback
	if got := s.evaluateCompaction(ctx, report); !got.Apply {
		t.Fatalf("fully priced compatible fallbacks should apply: %+v", got)
	}
}

// A route-only plugin returns PassRequest, so pricing must read the recorded
// verdict rather than a copy cached when someone returned a replacement.
//
// This is the multi-plugin shape in miniature: plugin A calls RouteRequest and
// passes; plugin B then evaluates compaction. Before the fix, PendingRoute was
// only refreshed by RequestMutationFunc — which fires on ReplaceRequest — so B
// priced the ORIGINAL provider and model while the request went elsewhere. The
// router fixture hid it by returning ReplaceRequest after routing, which is the
// v1 "return the same request" footgun v2 removes.
func TestEvaluateCompactionReadsRouteRecordedWithoutAReplacement(t *testing.T) {
	s := &Server{config: Config{Providers: provider.Config{Providers: map[string]provider.Provider{
		"initial": {Format: "openai"},
		"routed": {Format: "openai", Pricing: map[string]economics.ModelPricing{
			"cheap": {CacheReadUSDPerMTok: usd(0.5), CacheWriteUSDPerMTok: usd(1)},
		}},
	}}}}

	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { _ = rt.Close() })
	pp, err := plugin.NewPipeline(rt, plugin.PluginConfig{AllowUnapproved: true})
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}

	const reqID = 77
	// Plugin A's side effect, with NO replacement returned — the case the old
	// code could not see.
	rt.RecordRouteVerdictForTest(reqID, "plugin-a", "routed", "cheap")

	rs := &reqState{
		ID: reqID, Pipeline: pp,
		Provider: "initial", Model: "expensive",
		InitialProvider: "initial", InitialFormat: "openai",
		// PendingRoute deliberately nil: nothing returned a replacement.
	}
	ctx := context.WithValue(context.Background(), reqStateKey{}, rs)
	report := economics.CompactionReport{
		OriginalBytes: 100_000, FinalBytes: 5_000, EstimatedTokensRemoved: 95_000,
		EstimatedRewriteSpanTokens: 15_000, ExpectedApplications: 8, CandidateCount: 1, Source: "transformation",
	}

	got := s.evaluateCompaction(ctx, report)
	if !got.Apply {
		t.Fatalf("compaction was priced against the unrouted provider: %+v", got)
	}
}
