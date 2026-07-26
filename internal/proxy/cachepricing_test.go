package proxy

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/torana-edge/torana-edge/internal/economics"
	"github.com/torana-edge/torana-edge/internal/provider"
)

// anthropicRates are Claude Sonnet's shape: reads at 10% of base input, writes
// at 125%. The 12.5x ratio is what bounds a warming plugin to ~11 refreshes.
func anthropicRates() economics.ModelPricing {
	return economics.ModelPricing{
		InputUSDPerMTok:      f64(3.0),
		OutputUSDPerMTok:     f64(15.0),
		CacheReadUSDPerMTok:  f64(0.3),
		CacheWriteUSDPerMTok: f64(3.75),
	}
}

func pricingServer(t *testing.T, prov provider.Provider) *Server {
	t.Helper()
	cfg := provider.DefaultConfig()
	cfg.Providers = map[string]provider.Provider{"anth": prov}
	srv, err := New(Config{
		Port:       "0",
		Providers:  cfg,
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { srv.conversations.Close() })
	return srv
}

func askPricing(t *testing.T, srv *Server, payload string) cachePricingResponse {
	t.Helper()
	var out cachePricingResponse
	if err := json.Unmarshal([]byte(srv.cachePricing(context.Background(), payload)), &out); err != nil {
		t.Fatalf("decode pricing response: %v", err)
	}
	return out
}

func fullyConfigured() provider.Provider {
	return provider.Provider{
		URL:     "https://api.anthropic.com",
		Format:  "anthropic",
		Pricing: map[string]economics.ModelPricing{"claude-sonnet-4-5": anthropicRates()},
		Cache: &provider.CacheConfig{
			RefreshOnRead: true,
			Tiers: []provider.CacheTier{
				{TTLSeconds: 300, WriteMultiplier: 1.25, Marker: map[string]any{"type": "ephemeral"}},
				{TTLSeconds: 3600, WriteMultiplier: 2.0},
			},
		},
	}
}

// TestBreakEvenArithmetic is the number the whole feature turns on. Writes at
// 12.5x reads means refreshing pays for at most 11 refreshes; a wrong answer
// here makes a warming plugin lose money confidently.
func TestBreakEvenArithmetic(t *testing.T) {
	srv := pricingServer(t, fullyConfigured())
	got := askPricing(t, srv, `{"provider":"anth","model":"claude-sonnet-4-5"}`)

	if got.Status != "ok" {
		t.Fatalf("status = %q (%s), want ok", got.Status, got.Reason)
	}
	if got.WriteReadRatio != 12.5 {
		t.Errorf("WriteReadRatio = %v, want 12.5", got.WriteReadRatio)
	}
	if got.BreakEvenRefreshes != 11 {
		t.Errorf("BreakEvenRefreshes = %d, want 11 (floor(12.5-1))", got.BreakEvenRefreshes)
	}
	if !got.RefreshOnRead {
		t.Error("RefreshOnRead lost")
	}
	if got.ShortestTTLSeconds != 300 {
		t.Errorf("ShortestTTLSeconds = %d, want the 300s tier", got.ShortestTTLSeconds)
	}
	if got.WarmIntervalSeconds != 240 {
		t.Errorf("WarmIntervalSeconds = %d, want 240 (80%% of 300)", got.WarmIntervalSeconds)
	}
}

// TestUnavailableRatherThanGuessed is the discipline that keeps this honest.
// Every gap must be an explicit unavailable with a machine-readable reason: a
// plugin can only decline to spend money if the host refuses to invent numbers.
func TestUnavailableRatherThanGuessed(t *testing.T) {
	noCacheRates := fullyConfigured()
	noCacheRates.Pricing = map[string]economics.ModelPricing{
		"claude-sonnet-4-5": {InputUSDPerMTok: f64(3.0), OutputUSDPerMTok: f64(15.0)},
	}

	noCacheConfig := fullyConfigured()
	noCacheConfig.Cache = nil

	noPricing := fullyConfigured()
	noPricing.Pricing = nil

	for _, tc := range []struct {
		name    string
		prov    provider.Provider
		payload string
		want    string
	}{
		{"unknown provider", fullyConfigured(), `{"provider":"nope","model":"m"}`, pricingReasonUnknownProvider},
		{"no pricing", noPricing, `{"provider":"anth","model":"claude-sonnet-4-5"}`, pricingReasonNoPricing},
		{"no cache rates", noCacheRates, `{"provider":"anth","model":"claude-sonnet-4-5"}`, pricingReasonNoCacheRates},
		{"no cache semantics", noCacheConfig, `{"provider":"anth","model":"claude-sonnet-4-5"}`, pricingReasonNoCacheConfig},
		{"malformed payload", fullyConfigured(), `not json`, pricingReasonBadPayload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := askPricing(t, pricingServer(t, tc.prov), tc.payload)
			if got.Status != "unavailable" {
				t.Fatalf("status = %q, want unavailable", got.Status)
			}
			if got.Reason != tc.want {
				t.Errorf("reason = %q, want %q", got.Reason, tc.want)
			}
		})
	}
}

// TestUnknownModelIsUnpriced — pricing is keyed by exact model name with "*" as
// the provider default, and a model nobody priced must not inherit another's
// rates.
func TestUnknownModelIsUnpriced(t *testing.T) {
	got := askPricing(t, pricingServer(t, fullyConfigured()), `{"provider":"anth","model":"claude-opus-4-1"}`)
	if got.Status != "unavailable" || got.Reason != pricingReasonNoPricing {
		t.Errorf("unpriced model returned status=%q reason=%q", got.Status, got.Reason)
	}
}

// TestFreeCacheReportsNoAffordability — a zero read rate makes the ratio
// meaningless and would divide by zero. A plugin must not be able to read
// "infinite refreshes are free" out of a local model's zeroed pricing.
func TestFreeCacheReportsNoAffordability(t *testing.T) {
	free := fullyConfigured()
	free.Pricing = map[string]economics.ModelPricing{
		"local": {CacheReadUSDPerMTok: f64(0), CacheWriteUSDPerMTok: f64(0)},
	}
	got := askPricing(t, pricingServer(t, free), `{"provider":"anth","model":"local"}`)

	if got.Status != "ok" {
		t.Fatalf("status = %q (%s), want ok", got.Status, got.Reason)
	}
	if got.BreakEvenRefreshes != 0 || got.WriteReadRatio != 0 {
		t.Errorf("free cache reported ratio=%v refreshes=%d, want zeroes",
			got.WriteReadRatio, got.BreakEvenRefreshes)
	}
}

// TestNonRefreshableReportsSemanticsAnyway — OpenAI/DeepSeek-shaped providers.
// The prices are real and worth reporting; what matters is that RefreshOnRead
// is false, which is how a plugin knows refreshing cannot work here.
func TestNonRefreshableReportsSemanticsAnyway(t *testing.T) {
	auto := fullyConfigured()
	auto.Cache = &provider.CacheConfig{
		RefreshOnRead: false,
		Tiers:         []provider.CacheTier{{TTLSeconds: 300, WriteMultiplier: 1.0}},
	}
	got := askPricing(t, pricingServer(t, auto), `{"provider":"anth","model":"claude-sonnet-4-5"}`)

	if got.Status != "ok" {
		t.Fatalf("status = %q, want ok", got.Status)
	}
	if got.RefreshOnRead {
		t.Error("RefreshOnRead true for an automatic prefix cache")
	}
	if got.WarmIntervalSeconds != 0 {
		t.Errorf("WarmIntervalSeconds = %d for a non-refreshable cache, want 0", got.WarmIntervalSeconds)
	}
}
