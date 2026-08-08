package provider

import (
	"math"
	"testing"

	"github.com/torana-edge/torana-edge/internal/economics"
)

func TestPricingForExactThenWildcard(t *testing.T) {
	exact, fallback := 1.0, 2.0
	p := Provider{Pricing: map[string]economics.ModelPricing{
		"model-a": {InputUSDPerMTok: &exact},
		"*":       {InputUSDPerMTok: &fallback},
	}}
	got, ok := p.PricingFor("model-a")
	if !ok || got.InputUSDPerMTok == nil || *got.InputUSDPerMTok != exact {
		t.Fatalf("exact price=%+v ok=%v", got, ok)
	}
	got, ok = p.PricingFor("model-b")
	if !ok || got.InputUSDPerMTok == nil || *got.InputUSDPerMTok != fallback {
		t.Fatalf("fallback price=%+v ok=%v", got, ok)
	}
}

func TestConfigRejectsInvalidPricingRates(t *testing.T) {
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run(fmtFloat(bad), func(t *testing.T) {
			cfg := Config{
				Port: 8080,
				Providers: map[string]Provider{
					"p": {
						URL:    "https://example.com",
						Format: "openai",
						Pricing: map[string]economics.ModelPricing{
							"m": {InputUSDPerMTok: &bad},
						},
					},
				},
			}
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate accepted rate %v", bad)
			}
		})
	}

	zero := 0.0
	cfg := Config{
		Port: 8080,
		Providers: map[string]Provider{
			"p": {URL: "https://example.com", Format: "openai", Pricing: map[string]economics.ModelPricing{"m": {InputUSDPerMTok: &zero}}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected an explicitly free rate: %v", err)
	}
}

func fmtFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "nan"
	case math.IsInf(v, 1):
		return "positive-infinity"
	case math.IsInf(v, -1):
		return "negative-infinity"
	default:
		return "negative"
	}
}
