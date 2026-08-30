package proxy

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/economics"
	"github.com/torana-edge/torana-edge/internal/provider"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
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

// askPricing drives the REAL callback and decodes whichever arm it landed on:
// the value arm is the domain pricing body (status is genuine data: ok vs
// unavailable), a refusal is the framed classified HostError (caller bugs and
// operator gaps travel there).
func askPricing(t *testing.T, srv *Server, payload string) (cachePricingResponse, *pbv1.HostError) {
	t.Helper()
	res := srv.cachePricing(context.Background(), payload)
	if err := res.Validate(); err != nil {
		t.Fatalf("callback returned an invalid result: %v", err)
	}
	if res.Refusal() != nil {
		return cachePricingResponse{}, res.Refusal()
	}
	var out cachePricingResponse
	if err := json.Unmarshal(res.Value(), &out); err != nil {
		t.Fatalf("decode pricing response: %v (body %q)", err, string(res.Value()))
	}
	return out, nil
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
	got, herr := askPricing(t, srv, `{"provider":"anth","model":"claude-sonnet-4-5"}`)
	if herr != nil {
		t.Fatalf("priced query refused: %v", herr)
	}

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
// Every legitimate query gap — a model nobody priced, rates without cache
// semantics — is an explicit "unavailable" with a machine-readable reason in
// the VALUE arm: a plugin can only decline to spend money if the host refuses
// to invent numbers. These are query RESULTS, not transport failures, which
// is why they stay domain data while malformed input and unknown providers
// are framed refusals (see TestCachePricingClassification).
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
		{"no pricing", noPricing, `{"provider":"anth","model":"claude-sonnet-4-5"}`, pricingReasonNoPricing},
		{"no cache rates", noCacheRates, `{"provider":"anth","model":"claude-sonnet-4-5"}`, pricingReasonNoCacheRates},
		{"no cache semantics", noCacheConfig, `{"provider":"anth","model":"claude-sonnet-4-5"}`, pricingReasonNoCacheConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, herr := askPricing(t, pricingServer(t, tc.prov), tc.payload)
			if herr != nil {
				t.Fatalf("a legitimate query gap was framed as a refusal: %v", herr)
			}
			if got.Status != "unavailable" {
				t.Fatalf("status = %q, want unavailable", got.Status)
			}
			if got.Reason != tc.want {
				t.Errorf("reason = %q, want %q", got.Reason, tc.want)
			}
		})
	}
}

// TestCachePricingClassification is the F2 regression matrix over the REAL
// callback: caller bugs and operator gaps are FRAMED classified refusals
// (never smuggled through the value arm as a status string), while every
// legitimate query result stays a domain value.
func TestCachePricingClassification(t *testing.T) {
	srv := pricingServer(t, fullyConfigured())

	for _, tc := range []struct {
		name    string
		payload string
		want    pbv1.ErrorCode
	}{
		{"malformed JSON", `not json`, pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT},
		{"missing provider", `{"model":"claude-sonnet-4-5"}`, pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT},
		{"missing model", `{"provider":"anth"}`, pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT},
		{"unknown provider", `{"provider":"nope","model":"m"}`, pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, herr := askPricing(t, srv, tc.payload)
			if herr == nil {
				t.Fatalf("expected a framed refusal, got a value")
			}
			if herr.Code != tc.want {
				t.Errorf("code = %v, want %v", herr.Code, tc.want)
			}
		})
	}

	// The domain arms are untouched by the framing split.
	t.Run("unpriced model stays domain unavailable", func(t *testing.T) {
		got, herr := askPricing(t, srv, `{"provider":"anth","model":"claude-opus-4-1"}`)
		if herr != nil {
			t.Fatalf("unpriced model refused: %v", herr)
		}
		if got.Status != "unavailable" || got.Reason != pricingReasonNoPricing {
			t.Errorf("unpriced model returned status=%q reason=%q", got.Status, got.Reason)
		}
	})
	t.Run("priced stays domain ok", func(t *testing.T) {
		got, herr := askPricing(t, srv, `{"provider":"anth","model":"claude-sonnet-4-5"}`)
		if herr != nil {
			t.Fatalf("priced query refused: %v", herr)
		}
		if got.Status != "ok" || got.CacheReadUSDPerMTok != 0.3 {
			t.Errorf("priced query returned status=%q read=%v", got.Status, got.CacheReadUSDPerMTok)
		}
	})
}

// TestUnknownModelIsUnpriced — pricing is keyed by exact model name with "*" as
// the provider default, and a model nobody priced must not inherit another's
// rates.
func TestUnknownModelIsUnpriced(t *testing.T) {
	got, herr := askPricing(t, pricingServer(t, fullyConfigured()), `{"provider":"anth","model":"claude-opus-4-1"}`)
	if herr != nil {
		t.Fatalf("unpriced model refused: %v", herr)
	}
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
	got, herr := askPricing(t, pricingServer(t, free), `{"provider":"anth","model":"local"}`)
	if herr != nil {
		t.Fatalf("free-cache query refused: %v", herr)
	}

	if got.Status != "ok" {
		t.Fatalf("status = %q (%s), want ok", got.Status, got.Reason)
	}
	if got.BreakEvenRefreshes != 0 || got.WriteReadRatio != 0 {
		t.Errorf("free cache reported ratio=%v refreshes=%d, want zeroes",
			got.WriteReadRatio, got.BreakEvenRefreshes)
	}
}

// TestCachePricingFramesMarshalFailureAsInternal — a cache-tier marker that
// JSON cannot represent (here: NaN) is operator configuration, not guest
// input, and config validation does not reject it — so the host must frame
// the serialization failure as INTERNAL rather than smuggle an invented
// {"status":"unavailable"} through the value arm as if it were a legitimate
// pricing answer.
func TestCachePricingFramesMarshalFailureAsInternal(t *testing.T) {
	nanMarker := fullyConfigured()
	nanMarker.Cache.Tiers[0].Marker = map[string]any{"type": math.NaN()}

	// Config validation must NOT reject the marker (it is opaque); the host
	// learns of the failure only when the pricing body is serialized.
	srv, err := New(Config{
		Port: "0",
		Providers: func() provider.Config {
			c := provider.DefaultConfig()
			c.Providers = map[string]provider.Provider{"anth": nanMarker}
			return c
		}(),
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
	})
	if err != nil {
		t.Fatalf("config validation rejected the opaque marker: %v — the proof needs it to pass", err)
	}
	t.Cleanup(func() { srv.conversations.Close() })

	res := srv.cachePricing(context.Background(), `{"provider":"anth","model":"claude-sonnet-4-5"}`)
	if err := res.Validate(); err != nil {
		t.Fatalf("callback returned an invalid result: %v", err)
	}
	if res.Refusal() == nil {
		t.Fatalf("a serialization failure was framed as a value: %q", string(res.Value()))
	}
	if res.Refusal().Code != pbv1.ErrorCode_ERROR_CODE_INTERNAL {
		t.Errorf("code = %v, want INTERNAL — an unrepresentable pricing body is a host invariant, not a query result", res.Refusal().Code)
	}
	if !strings.Contains(res.Refusal().Message, "encode pricing response") {
		t.Errorf("message = %q, want it to name the encode failure", res.Refusal().Message)
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
	got, herr := askPricing(t, pricingServer(t, auto), `{"provider":"anth","model":"claude-sonnet-4-5"}`)
	if herr != nil {
		t.Fatalf("non-refreshable query refused: %v", herr)
	}

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
