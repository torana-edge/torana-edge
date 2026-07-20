package provider

import (
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
