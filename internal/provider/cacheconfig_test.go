package provider

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// anthropicShaped is the two-tier configuration Anthropic sells: 5 minutes at
// 1.25x the base input rate, 1 hour at 2.0x, both refreshed by reads.
func anthropicShaped() *CacheConfig {
	return &CacheConfig{
		RefreshOnRead: true,
		Tiers: []CacheTier{
			{TTLSeconds: 300, WriteMultiplier: 1.25, Marker: map[string]any{"type": "ephemeral"}},
			{TTLSeconds: 3600, WriteMultiplier: 2.0, Marker: map[string]any{"type": "ephemeral", "ttl": "1h"}},
		},
	}
}

func provWith(c *CacheConfig) Provider {
	return Provider{URL: "https://example.com", Format: "anthropic", Cache: c}
}

// TestNilCacheIsValid — cache semantics are optional, and unknown must stay a
// legal state. Anything that would spend money then has to decline rather than
// guess.
func TestNilCacheIsValid(t *testing.T) {
	if err := provWith(nil).ValidateCache("p"); err != nil {
		t.Errorf("a provider without cache config was rejected: %v", err)
	}
}

func TestValidCacheAccepted(t *testing.T) {
	if err := provWith(anthropicShaped()).ValidateCache("p"); err != nil {
		t.Errorf("a well-formed cache config was rejected: %v", err)
	}
}

// TestIntervalBeyondTTLRejected is the guardrail that matters most. An interval
// at or past the TTL never refreshes anything but still pays for every request
// it sends, so it must fail at config time rather than on a bill.
func TestIntervalBeyondTTLRejected(t *testing.T) {
	for _, interval := range []int{300, 301, 600} {
		c := anthropicShaped()
		c.WarmIntervalSeconds = interval
		err := provWith(c).ValidateCache("p")
		if err == nil {
			t.Errorf("warm_interval_seconds=%d (shortest TTL 300) was accepted", interval)
			continue
		}
		if !strings.Contains(err.Error(), "expired") {
			t.Errorf("error for interval %d does not explain the consequence: %v", interval, err)
		}
	}
}

func TestIntervalWithinTTLAccepted(t *testing.T) {
	c := anthropicShaped()
	c.WarmIntervalSeconds = 240
	if err := provWith(c).ValidateCache("p"); err != nil {
		t.Errorf("a valid interval was rejected: %v", err)
	}
}

func TestMalformedTiersRejected(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		cfg        *CacheConfig
	}{
		{
			name: "no tiers", want: "at least one tier",
			cfg: &CacheConfig{RefreshOnRead: true},
		},
		{
			name: "zero ttl", want: "ttl_seconds must be positive",
			cfg: &CacheConfig{Tiers: []CacheTier{{TTLSeconds: 0, WriteMultiplier: 1.25}}},
		},
		{
			name: "negative multiplier", want: "write_multiplier cannot be negative",
			cfg: &CacheConfig{Tiers: []CacheTier{{TTLSeconds: 300, WriteMultiplier: -1}}},
		},
		{
			name: "duplicate ttls", want: "two tiers with ttl_seconds",
			cfg: &CacheConfig{Tiers: []CacheTier{
				{TTLSeconds: 300, WriteMultiplier: 1.25},
				{TTLSeconds: 300, WriteMultiplier: 2.0},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := provWith(tc.cfg).ValidateCache("p")
			if err == nil {
				t.Fatal("expected rejection")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// TestShortestTierIsWhatWarmingRaces — a refresh loop must race the shortest
// lifetime, regardless of the order tiers were declared in.
func TestShortestTierIsWhatWarmingRaces(t *testing.T) {
	c := &CacheConfig{RefreshOnRead: true, Tiers: []CacheTier{
		{TTLSeconds: 3600, WriteMultiplier: 2.0},
		{TTLSeconds: 300, WriteMultiplier: 1.25},
	}}
	got, ok := c.ShortestTier()
	if !ok || got.TTLSeconds != 300 {
		t.Errorf("ShortestTier = %+v (ok=%v), want the 300s tier", got, ok)
	}
	if _, ok := (*CacheConfig)(nil).ShortestTier(); ok {
		t.Error("nil config reported a tier")
	}
}

// TestWarmIntervalDefaultsBelowTTL — the default has to leave room for latency
// and clock skew, or a refresh arrives after the entry has already lapsed.
func TestWarmIntervalDefaultsBelowTTL(t *testing.T) {
	got := anthropicShaped().WarmInterval()
	if got != 240*time.Second {
		t.Errorf("WarmInterval = %v, want 240s (80%% of the 300s tier)", got)
	}
	if got >= 300*time.Second {
		t.Error("the default interval does not leave headroom before expiry")
	}
}

// TestWarmIntervalZeroWhenNotRefreshable is the OpenAI/DeepSeek case: automatic
// prefix caching with no lifetime the caller controls. There is nothing a
// periodic request can keep alive, so warming must be impossible, not merely
// discouraged.
func TestWarmIntervalZeroWhenNotRefreshable(t *testing.T) {
	c := anthropicShaped()
	c.RefreshOnRead = false
	if got := c.WarmInterval(); got != 0 {
		t.Errorf("WarmInterval = %v for a non-refreshable cache, want 0", got)
	}
	if got := (*CacheConfig)(nil).WarmInterval(); got != 0 {
		t.Errorf("WarmInterval = %v for nil config, want 0", got)
	}
	noTiers := &CacheConfig{RefreshOnRead: true}
	if got := noTiers.WarmInterval(); got != 0 {
		t.Errorf("WarmInterval = %v with no tiers, want 0", got)
	}
}

// TestExplicitIntervalOverridesDefault — the per-provider override the operator
// sets must win over the derived default.
func TestExplicitIntervalOverridesDefault(t *testing.T) {
	c := anthropicShaped()
	c.WarmIntervalSeconds = 120
	if got := c.WarmInterval(); got != 120*time.Second {
		t.Errorf("WarmInterval = %v, want the configured 120s", got)
	}
}

// TestMarkerRoundTripsOpaquely — Torana places the breakpoint marker but never
// interprets it, so provider-specific shapes must survive config verbatim.
func TestMarkerRoundTripsOpaquely(t *testing.T) {
	raw := `{"refresh_on_read":true,"tiers":[
		{"ttl_seconds":3600,"write_multiplier":2,"marker":{"type":"ephemeral","ttl":"1h","vendor_extra":{"nested":true}}}]}`
	var c CacheConfig
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	marker := c.Tiers[0].Marker
	if marker["ttl"] != "1h" || marker["type"] != "ephemeral" {
		t.Errorf("marker lost fields: %+v", marker)
	}
	if _, ok := marker["vendor_extra"].(map[string]any); !ok {
		t.Errorf("marker dropped an unrecognised nested field: %+v", marker)
	}
}

// TestConfigValidateReachesCacheValidation — the check has to be reachable from
// the top-level entry point, not just callable in isolation.
func TestConfigValidateReachesCacheValidation(t *testing.T) {
	bad := anthropicShaped()
	bad.WarmIntervalSeconds = 9999

	cfg := Config{
		Port:      8080,
		Providers: map[string]Provider{"anth": provWith(bad)},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Config.Validate did not reach ValidateCache")
	}
	if !strings.Contains(err.Error(), "warm_interval_seconds") {
		t.Errorf("unexpected error: %v", err)
	}
}
