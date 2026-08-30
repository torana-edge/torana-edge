package provider

import "testing"

// TestUnauthenticatedFallbacksAreReported: a fallback with no credential of its
// own receives an unauthenticated request and answers 401, which is not
// retryable — so it becomes the caller's response and failover turns a
// recoverable 429 into a hard failure. The operator should hear that at startup
// rather than from the first 429 in production.
func TestUnauthenticatedFallbacksAreReported(t *testing.T) {
	cfg := Config{Providers: map[string]Provider{
		"primary":   {URL: "https://a.example", Format: "openai", Auth: ProviderAuth{Mode: "caller"}, Fallback: []string{"bare", "keyed", "caller"}},
		"bare":      {URL: "https://b.example", Format: "openai", Auth: ProviderAuth{Mode: "none"}},
		"keyed":     {URL: "https://c.example", Format: "openai", Auth: ProviderAuth{Mode: "credential", Credential: "c"}},
		"caller":    {URL: "https://d.example", Format: "openai", Auth: ProviderAuth{Mode: "caller"}},
		"unrelated": {URL: "https://e.example", Format: "openai", Auth: ProviderAuth{Mode: "none"}},
	}}

	got := cfg.UnauthenticatedFallbacks()
	if len(got) != 1 || got[0] != "bare" {
		t.Errorf("got %v, want exactly [bare] — 'keyed' has its own credential, 'optedin' "+
			"uses caller auth, and 'unrelated' is never used as a fallback", got)
	}
}

// A provider that is never a fallback target is not this check's business, even
// with no credential — plenty of providers legitimately need none.
func TestUnauthenticatedFallbacksIgnoresNonFallbacks(t *testing.T) {
	cfg := Config{Providers: map[string]Provider{
		"local": {URL: "http://localhost:11434", Format: "openai", Auth: ProviderAuth{Mode: "none"}},
	}}
	if got := cfg.UnauthenticatedFallbacks(); len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}
