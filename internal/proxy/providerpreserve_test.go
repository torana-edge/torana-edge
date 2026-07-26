package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/torana-edge/torana-edge/internal/economics"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func f64(v float64) *float64 { return &v }

// pricedServer builds a server whose "primary" provider carries pricing —
// the state the settings form used to destroy.
func pricedServer(t *testing.T) *Server {
	t.Helper()
	provCfg := provider.DefaultConfig()
	provCfg.Providers = map[string]provider.Provider{
		"primary": {
			URL:    "https://api.example.com",
			Format: "openai",
			Pricing: map[string]economics.ModelPricing{
				"my-model": {
					InputUSDPerMTok:      f64(1.0),
					OutputUSDPerMTok:     f64(4.0),
					CacheReadUSDPerMTok:  f64(0.1),
					CacheWriteUSDPerMTok: f64(1.25),
				},
			},
		},
	}
	srv, err := New(Config{
		Port:       strconv.Itoa(0),
		Providers:  provCfg,
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func putConfig(t *testing.T, srv *Server, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := localControlPlaneRequest(http.MethodPut, "/_torana/api/config", bytes.NewReader(b))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Torana-Local-Request", "1")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func pricingOf(t *testing.T, srv *Server, provName, model string) (economics.ModelPricing, bool) {
	t.Helper()
	p, ok := srv.GetConfig().Providers.Providers[provName]
	if !ok {
		t.Fatalf("provider %q missing after save", provName)
	}
	return p.PricingFor(model)
}

// TestSettingsSaveKeepsPricing is the regression test for the bug that shipped:
// the settings form rebuilds each provider from the six fields it renders and
// replaces the whole providers map, so an ordinary Save silently deleted the
// pricing that the compactor's economic gate depends on.
func TestSettingsSaveKeepsPricing(t *testing.T) {
	srv := pricedServer(t)

	// Exactly what the settings form sends: no "pricing" key anywhere.
	rec := putConfig(t, srv, map[string]any{
		"port": 8080,
		"providers": map[string]any{
			"primary": map[string]any{
				"url":    "https://api.example.com",
				"format": "openai",
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", rec.Code, rec.Body)
	}

	got, ok := pricingOf(t, srv, "primary", "my-model")
	if !ok {
		t.Fatal("pricing was erased by a settings save")
	}
	if got.CacheReadUSDPerMTok == nil || *got.CacheReadUSDPerMTok != 0.1 {
		t.Errorf("cache_read rate = %v, want 0.1", got.CacheReadUSDPerMTok)
	}
	if got.CacheWriteUSDPerMTok == nil || *got.CacheWriteUSDPerMTok != 1.25 {
		t.Errorf("cache_write rate = %v, want 1.25", got.CacheWriteUSDPerMTok)
	}
}

// TestSettingsSaveAppliesManagedFields guards the other side: preserving
// unmanaged fields must not make managed ones sticky too.
func TestSettingsSaveAppliesManagedFields(t *testing.T) {
	srv := pricedServer(t)

	rec := putConfig(t, srv, map[string]any{
		"port": 8080,
		"providers": map[string]any{
			"primary": map[string]any{
				"url":    "https://api.changed.example.com",
				"format": "anthropic",
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", rec.Code, rec.Body)
	}

	p := srv.GetConfig().Providers.Providers["primary"]
	if p.URL != "https://api.changed.example.com" {
		t.Errorf("url = %q, was not updated", p.URL)
	}
	if p.Format != "anthropic" {
		t.Errorf("format = %q, was not updated", p.Format)
	}
}

// TestPricingRemainsEditable pins that preservation is presence-based, not
// unconditional: a client that actually manages pricing must still be able to
// change and clear it, or the future pricing editor cannot work.
func TestPricingRemainsEditable(t *testing.T) {
	srv := pricedServer(t)

	rec := putConfig(t, srv, map[string]any{
		"port": 8080,
		"providers": map[string]any{
			"primary": map[string]any{
				"url":     "https://api.example.com",
				"format":  "openai",
				"pricing": map[string]any{"my-model": map[string]any{"cache_read_usd_per_mtok": 0.5}},
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", rec.Code, rec.Body)
	}
	got, ok := pricingOf(t, srv, "primary", "my-model")
	if !ok {
		t.Fatal("pricing missing after an explicit write")
	}
	if got.CacheReadUSDPerMTok == nil || *got.CacheReadUSDPerMTok != 0.5 {
		t.Fatalf("cache_read rate = %v, want 0.5 — explicit writes must win", got.CacheReadUSDPerMTok)
	}

	// An explicit null clears it.
	rec = putConfig(t, srv, map[string]any{
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
		t.Fatalf("PUT status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if _, ok := pricingOf(t, srv, "primary", "my-model"); ok {
		t.Error("explicit null did not clear pricing")
	}
}

// TestNewProviderNeedsNoStoredFields — a provider that did not exist before has
// nothing to preserve and must not inherit another provider's settings.
func TestNewProviderNeedsNoStoredFields(t *testing.T) {
	srv := pricedServer(t)

	rec := putConfig(t, srv, map[string]any{
		"port": 8080,
		"providers": map[string]any{
			"primary":   map[string]any{"url": "https://api.example.com", "format": "openai"},
			"secondary": map[string]any{"url": "https://api2.example.com", "format": "openai"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if p := srv.GetConfig().Providers.Providers["secondary"]; len(p.Pricing) != 0 {
		t.Errorf("new provider inherited pricing: %+v", p.Pricing)
	}
}

// TestSettingsSaveKeepsCacheSemantics — cache config is the second setting the
// form does not render, and losing it would silently disable the cache plugins
// exactly the way losing pricing disabled the compactor's gate.
func TestSettingsSaveKeepsCacheSemantics(t *testing.T) {
	srv := pricedServer(t)

	// Declare cache semantics the way an operator would, then save Settings.
	rec := putConfig(t, srv, map[string]any{
		"port": 8080,
		"providers": map[string]any{
			"primary": map[string]any{
				"url":    "https://api.example.com",
				"format": "openai",
				"cache": map[string]any{
					"refresh_on_read": true,
					"tiers": []any{
						map[string]any{"ttl_seconds": 300, "write_multiplier": 1.25,
							"marker": map[string]any{"type": "ephemeral"}},
					},
				},
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", rec.Code, rec.Body)
	}

	// Now a plain settings save, which sends no cache key at all.
	rec = putConfig(t, srv, map[string]any{
		"port": 8080,
		"providers": map[string]any{
			"primary": map[string]any{"url": "https://api.example.com", "format": "openai"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("second PUT status = %d, want 200: %s", rec.Code, rec.Body)
	}

	p := srv.GetConfig().Providers.Providers["primary"]
	if p.Cache == nil {
		t.Fatal("cache semantics were erased by a settings save")
	}
	if !p.Cache.RefreshOnRead || len(p.Cache.Tiers) != 1 {
		t.Errorf("cache config survived but was mangled: %+v", p.Cache)
	}
	if got := p.Cache.Tiers[0].Marker["type"]; got != "ephemeral" {
		t.Errorf("tier marker = %v, want the opaque provider value", got)
	}
}

// TestProviderFieldsSent covers the presence detection directly, including the
// case that matters most: a zero value that was genuinely sent.
func TestProviderFieldsSent(t *testing.T) {
	sent := providerFieldsSent([]byte(`{"providers":{"a":{"url":"x","pricing":null},"b":{"url":"y"}}}`))

	if _, ok := sent["a"]["pricing"]; !ok {
		t.Error("an explicit null pricing was not detected as sent")
	}
	if _, ok := sent["b"]["pricing"]; ok {
		t.Error("an omitted pricing was reported as sent")
	}
	if got := providerFieldsSent([]byte(`not json`)); got != nil {
		t.Errorf("unparseable body = %v, want nil", got)
	}
}
