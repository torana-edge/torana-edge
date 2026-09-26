package provider

import (
	"encoding/json"
	"testing"

	"github.com/torana-edge/torana-edge/internal/economics"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func TestDeclaredModelCapabilitiesRoundTripAndUnknownRefusal(t *testing.T) {
	cfg := DefaultConfig()
	upstream := cfg.Providers["anthropic"]
	price := 3.25
	window := uint32(200000)
	upstream.Models = map[string]ModelCapabilitiesConfig{
		"declared": {Effort: &ModelEffortConfig{Levels: []string{"low", "high"}},
			ContextWindowTokens: &window, Pricing: &economics.ModelPricing{InputUSDPerMTok: &price}},
		"unpriced":    {},
		"exact-price": {},
	}
	upstream.Pricing = map[string]economics.ModelPricing{"exact-price": {InputUSDPerMTok: &price}}
	cfg.Providers["anthropic"] = upstream
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	got, ok := decoded.ModelCapabilities("anthropic", "declared")
	if !ok || got.Format != "anthropic" || got.GetContextWindowTokens() != window || got.Pricing == nil || got.Pricing.GetInputUsdPerMtok() != price || len(got.EffortLevels) != 2 || got.EffortLevels[1] != pb.Effort_EFFORT_HIGH {
		t.Fatalf("declared capability: %+v, found %v", got, ok)
	}
	if got, ok := decoded.ModelCapabilities("anthropic", "unpriced"); !ok || got.Pricing != nil || len(got.EffortLevels) != 0 {
		t.Fatalf("unpriced capability: %+v, found %v", got, ok)
	}
	if got, ok := decoded.ModelCapabilities("anthropic", "exact-price"); !ok || got.Pricing == nil || got.Pricing.GetInputUsdPerMtok() != price {
		t.Fatalf("exact price was not reused: %+v, found %v", got, ok)
	}
	for _, key := range [][2]string{{"anthropic", "missing"}, {"missing", "declared"}} {
		if got, ok := decoded.ModelCapabilities(key[0], key[1]); ok || got != nil {
			t.Fatalf("undeclared capability leaked: %+v", got)
		}
	}
}

func TestModelCapabilitiesRejectConflictingPriceAndWhitespaceKey(t *testing.T) {
	first, second := 1.0, 2.0
	for _, model := range []string{" model", "model "} {
		cfg := DefaultConfig()
		upstream := cfg.Providers["anthropic"]
		upstream.Models = map[string]ModelCapabilitiesConfig{model: {}}
		cfg.Providers["anthropic"] = upstream
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted whitespace model key %q", model)
		}
	}
	cfg := DefaultConfig()
	upstream := cfg.Providers["anthropic"]
	upstream.Pricing = map[string]economics.ModelPricing{"model": {InputUSDPerMTok: &first}}
	upstream.Models = map[string]ModelCapabilitiesConfig{"model": {Pricing: &economics.ModelPricing{InputUSDPerMTok: &second}}}
	cfg.Providers["anthropic"] = upstream
	if err := cfg.Validate(); err == nil {
		t.Fatal("conflicting price declarations accepted")
	}
}

func TestModelCapabilitiesRejectInvalidLevels(t *testing.T) {
	for _, levels := range [][]string{{"high", "high"}, {"turbo"}} {
		cfg := DefaultConfig()
		upstream := cfg.Providers["anthropic"]
		upstream.Models = map[string]ModelCapabilitiesConfig{"m": {Effort: &ModelEffortConfig{Levels: levels}}}
		cfg.Providers["anthropic"] = upstream
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid levels %v accepted", levels)
		}
	}
}

func TestGeminiEffortRequiresNativeMapping(t *testing.T) {
	cfg := DefaultConfig()
	upstream := cfg.Providers["gemini"]
	upstream.Models = map[string]ModelCapabilitiesConfig{"m": {Effort: &ModelEffortConfig{Levels: []string{"low"}}}}
	cfg.Providers["gemini"] = upstream
	if err := cfg.Validate(); err == nil {
		t.Fatal("unmapped Gemini effort accepted")
	}
	upstream.Models["m"] = ModelCapabilitiesConfig{Effort: &ModelEffortConfig{
		Levels: []string{"low"}, Gemini: map[string]GeminiThinkingConfig{"low": {ThinkingLevel: "LOW"}},
	}}
	cfg.Providers["gemini"] = upstream
	if err := cfg.Validate(); err != nil {
		t.Fatalf("mapped Gemini effort refused: %v", err)
	}
}
