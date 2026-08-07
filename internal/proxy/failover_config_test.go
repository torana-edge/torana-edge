package proxy

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestFallbacksComeOnlyFromTheSelectedProvider(t *testing.T) {
	cfg := provider.Config{
		Providers: map[string]provider.Provider{
			"primary": {Fallback: []string{"declared"}},
			"none":    {},
		},
		Plugins: provider.PluginsConfig{Config: map[string]json.RawMessage{
			"failover": json.RawMessage(`{"allowed_fallbacks":["injected"]}`),
		}},
	}
	if got := fallbackNamesForProvider("primary", cfg); !reflect.DeepEqual(got, []string{"declared"}) {
		t.Fatalf("primary fallbacks = %v, want provider declaration", got)
	}
	if got := fallbackNamesForProvider("none", cfg); len(got) != 0 {
		t.Fatalf("provider without fallbacks gained %v from plugin config", got)
	}
}
