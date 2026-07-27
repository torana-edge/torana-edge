package provider

import "testing"

// TestUnauthenticatedFallbacksAreReported: a fallback with no credential of its
// own receives an unauthenticated request and answers 401, which is not
// retryable — so it becomes the caller's response and failover turns a
// recoverable 429 into a hard failure. The operator should hear that at startup
// rather than from the first 429 in production.
func TestUnauthenticatedFallbacksAreReported(t *testing.T) {
	cfg := Config{Providers: map[string]Provider{
		"primary":   {URL: "https://a.example", Format: "openai", APIKeyEnv: "A", Fallback: []string{"bare", "keyed", "optedin"}},
		"bare":      {URL: "https://b.example", Format: "openai"},
		"keyed":     {URL: "https://c.example", Format: "openai", APIKeyEnv: "C"},
		"optedin":   {URL: "https://d.example", Format: "openai", ForwardCallerCredential: true},
		"unrelated": {URL: "https://e.example", Format: "openai"},
	}}

	got := cfg.UnauthenticatedFallbacks()
	if len(got) != 1 || got[0] != "bare" {
		t.Errorf("got %v, want exactly [bare] — 'keyed' has its own credential, 'optedin' "+
			"declared forwarding, and 'unrelated' is never used as a fallback", got)
	}
}

// A provider that is never a fallback target is not this check's business, even
// with no credential — plenty of providers legitimately need none.
func TestUnauthenticatedFallbacksIgnoresNonFallbacks(t *testing.T) {
	cfg := Config{Providers: map[string]Provider{
		"local": {URL: "http://localhost:11434", Format: "openai"},
	}}
	if got := cfg.UnauthenticatedFallbacks(); len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}
