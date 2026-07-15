package main

import "testing"

// TestTruncateForPromptUnboundedByDefault: with no configured limit (maxChars
// <= 0) the complete tool output is sent to the summarizer — no silent
// middle-dropping. Regression guard for the removed hardcoded 14000 cap.
func TestTruncateForPromptUnboundedByDefault(t *testing.T) {
	big := make([]byte, 100_000)
	for i := range big {
		big[i] = 'x'
	}
	content := string(big)

	if got := truncateForPrompt(content, 0); got != content {
		t.Fatalf("maxChars=0 must pass content through unchanged; got %d chars, want %d", len(got), len(content))
	}
	if got := truncateForPrompt(content, -5); got != content {
		t.Fatalf("negative maxChars must be unbounded; got %d chars", len(got))
	}
}

// TestTruncateForPromptBoundedWhenConfigured: a positive cap keeps head+tail
// and drops the middle, staying within the configured budget.
func TestTruncateForPromptBoundedWhenConfigured(t *testing.T) {
	content := ""
	for i := 0; i < 1000; i++ {
		content += "abcdefghij" // 10k chars
	}
	out := truncateForPrompt(content, 100)
	if len(out) >= len(content) {
		t.Fatalf("expected truncation below %d, got %d", len(content), len(out))
	}
	if !containsMarker(out) {
		t.Fatalf("truncated output missing head/tail marker: %q", out[:min(80, len(out))])
	}
	// Short content under the cap is returned intact.
	if got := truncateForPrompt("small", 100); got != "small" {
		t.Fatalf("content under cap must be intact, got %q", got)
	}
}

func containsMarker(s string) bool {
	const marker = "... [truncated] ..."
	for i := 0; i+len(marker) <= len(s); i++ {
		if s[i:i+len(marker)] == marker {
			return true
		}
	}
	return false
}
