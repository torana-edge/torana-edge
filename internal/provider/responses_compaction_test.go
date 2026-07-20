package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadResponsesCompaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	const body = `{"providers":{"openai":{"url":"https://api.openai.com","format":"openai","responses_compaction":{"compact_threshold":12345}}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Providers["openai"].ResponsesCompaction
	if got == nil || got.CompactThreshold != 12345 {
		t.Fatalf("unexpected responses compaction config: %+v", got)
	}
}

func TestLoadRejectsInvalidResponsesCompaction(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"zero", `{"providers":{"p":{"url":"https://example.com","format":"openai","responses_compaction":{"compact_threshold":0}}}}`, "must be positive"},
		{"negative", `{"providers":{"p":{"url":"https://example.com","format":"openai","responses_compaction":{"compact_threshold":-1}}}}`, "must be positive"},
		{"wrong format", `{"providers":{"p":{"url":"https://example.com","format":"anthropic","responses_compaction":{"compact_threshold":10}}}}`, "requires format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestResponsesCompactionAbsentIsDisabled(t *testing.T) {
	p := Provider{Format: "openai"}
	if err := p.ValidateResponsesCompaction("openai"); err != nil {
		t.Fatalf("absent config should be valid: %v", err)
	}
	if p.ResponsesCompaction != nil {
		t.Fatal("absent config should remain nil")
	}
}
