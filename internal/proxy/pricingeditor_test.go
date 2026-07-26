package proxy

import (
	"net/http"
	"testing"

	"github.com/torana-edge/torana-edge/internal/economics"
)

// The settings form now manages pricing, which puts it in tension with the
// server-side preservation that stops forms losing config they cannot render.
// These pin the resolution: an omitted field is preserved, an explicit null
// clears. Get this wrong in either direction and an operator either cannot
// delete a rate, or loses every rate the moment they touch Settings.

// TestPricingClearedByExplicitNull — the pricing editor sends null for an
// emptied table. If preservation won here, deleting a rate would silently fail.
func TestPricingClearedByExplicitNull(t *testing.T) {
	srv := pricedServer(t)

	rec := putConfig(t, srv, map[string]any{
		"port": 8080,
		"providers": map[string]any{
			"primary": map[string]any{
				"url":     "https://api.example.com",
				"format":  "openai",
				"pricing": nil,
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d: %s", rec.Code, rec.Body)
	}
	if p := srv.GetConfig().Providers.Providers["primary"]; len(p.Pricing) != 0 {
		t.Errorf("pricing survived an explicit null: %+v — an operator could not delete a rate", p.Pricing)
	}
}

// TestPricingReplacedWholesale — the editor sends the whole map, so a model
// removed from the table must disappear rather than linger from a merge.
func TestPricingReplacedWholesale(t *testing.T) {
	srv := pricedServer(t)

	// Two models configured...
	rec := putConfig(t, srv, map[string]any{
		"port": 8080,
		"providers": map[string]any{
			"primary": map[string]any{
				"url": "https://api.example.com", "format": "openai",
				"pricing": map[string]any{
					"model-a": map[string]any{"cache_read_usd_per_mtok": 0.1},
					"model-b": map[string]any{"cache_read_usd_per_mtok": 0.2},
				},
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("first PUT: %d %s", rec.Code, rec.Body)
	}

	// ...then one removed from the table.
	rec = putConfig(t, srv, map[string]any{
		"port": 8080,
		"providers": map[string]any{
			"primary": map[string]any{
				"url": "https://api.example.com", "format": "openai",
				"pricing": map[string]any{
					"model-a": map[string]any{"cache_read_usd_per_mtok": 0.1},
				},
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("second PUT: %d %s", rec.Code, rec.Body)
	}

	p := srv.GetConfig().Providers.Providers["primary"]
	if _, still := p.Pricing["model-b"]; still {
		t.Error("a model removed from the pricing table survived the save")
	}
	if _, ok := p.Pricing["model-a"]; !ok {
		t.Error("the remaining model was lost")
	}
}

// TestAbsentRateStaysAbsent pins the distinction the editor depends on: an
// empty cell means "unknown", not zero. Anything that spends money declines on
// unknown but treats zero as an explicitly free rate, so conflating them would
// make a feature act when it should have refused.
func TestAbsentRateStaysAbsent(t *testing.T) {
	srv := pricedServer(t)

	rec := putConfig(t, srv, map[string]any{
		"port": 8080,
		"providers": map[string]any{
			"primary": map[string]any{
				"url": "https://api.example.com", "format": "openai",
				"pricing": map[string]any{
					// Only a cache-read rate; the others were left blank.
					"m": map[string]any{"cache_read_usd_per_mtok": 0.3},
				},
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}

	got, ok := srv.GetConfig().Providers.Providers["primary"].PricingFor("m")
	if !ok {
		t.Fatal("pricing missing")
	}
	if got.CacheReadUSDPerMTok == nil || *got.CacheReadUSDPerMTok != 0.3 {
		t.Errorf("cache read = %v, want 0.3", got.CacheReadUSDPerMTok)
	}
	if got.CacheWriteUSDPerMTok != nil {
		t.Errorf("a blank cell became %v; blank must stay unknown, not zero", *got.CacheWriteUSDPerMTok)
	}
	if got.InputUSDPerMTok != nil {
		t.Errorf("a blank input rate became %v", *got.InputUSDPerMTok)
	}
}

// TestExplicitZeroRateIsKept — a local model really is free, and that must be
// expressible. It is the counterpart to the test above.
func TestExplicitZeroRateIsKept(t *testing.T) {
	srv := pricedServer(t)

	rec := putConfig(t, srv, map[string]any{
		"port": 8080,
		"providers": map[string]any{
			"primary": map[string]any{
				"url": "https://api.example.com", "format": "openai",
				"pricing": map[string]any{
					"local": map[string]any{
						"input_usd_per_mtok":       0,
						"cache_read_usd_per_mtok":  0,
						"cache_write_usd_per_mtok": 0,
					},
				},
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}

	got, ok := srv.GetConfig().Providers.Providers["primary"].PricingFor("local")
	if !ok {
		t.Fatal("pricing missing")
	}
	if got.CacheReadUSDPerMTok == nil {
		t.Fatal("an explicit zero rate was dropped — a free local model must be expressible")
	}
	if *got.CacheReadUSDPerMTok != 0 {
		t.Errorf("cache read = %v, want 0", *got.CacheReadUSDPerMTok)
	}
	var _ economics.ModelPricing = got
}
