package proxy

import (
	"testing"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// SetProviders must REPORT a rejection, not just log it.
//
// Control-plane handlers persist a candidate to disk before publishing it to
// the live server. While this returned nothing, a rejected credential registry
// was logged and swallowed: the write had already landed, the handler still
// answered 200, and disk and memory disagreed from then on. The operator was
// told their change took effect while the running proxy kept the old one — and
// the next restart would load the configuration that had just been rejected.
func TestSetProvidersReportsARejectedCredentialRegistry(t *testing.T) {
	srv := &Server{rateLimiter: NewRateLimiter(0, 0)}
	defer srv.rateLimiter.Close()

	good := provider.DefaultConfig()
	if err := srv.SetProviders(good); err != nil {
		t.Fatalf("a valid configuration was rejected: %v", err)
	}
	if got := len(srv.GetConfig().Providers.Providers); got == 0 {
		t.Fatal("the accepted configuration was not published")
	}

	// A source with no type cannot build a provider, so the registry is
	// refused — the same class of rejection a real misconfiguration produces.
	bad := provider.DefaultConfig()
	bad.Credentials.Sources = map[string]provider.CredentialSource{"broken": {Type: ""}}
	bad.Providers = map[string]provider.Provider{
		"only-in-the-rejected-config": {URL: "https://nope.example", Format: "openai"},
	}

	err := srv.SetProviders(bad)
	if err == nil {
		t.Fatal("a configuration whose credential registry cannot be built was accepted " +
			"silently; the caller has already written it to disk and will answer 200")
	}

	// And the live config must be untouched by the rejected candidate.
	live := srv.GetConfig().Providers
	if _, leaked := live.Providers["only-in-the-rejected-config"]; leaked {
		t.Error("the rejected configuration was published to the live server anyway")
	}
	if _, ok := live.Providers["deepseek"]; !ok {
		t.Error("the previously accepted configuration was lost by a rejected update")
	}
}
