package main

import (
	"os"
	"strings"
	"testing"
)

// CONTRIBUTING is where a new plugin author is sent for repository-specific
// rules. Keep its schema and storage descriptions aligned with the contracts
// enforced by internal/plugin rather than letting obsolete implementation
// history become author guidance again.
func TestContributingDescribesCurrentPluginContracts(t *testing.T) {
	body, err := os.ReadFile("CONTRIBUTING.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(body)
	for _, required := range []string{
		"Schema document validates the complete stored configuration",
		"The original `{\"fields\":[...]}` format remains supported",
		"`env.cache_*` is a plugin-private, cross-request TTL cache",
		"`env.shared_cache_*` is the separately granted shared flat keyspace",
		"reads return classified `NOT_FOUND` results, not ambiguous empty values",
	} {
		if !strings.Contains(doc, required) {
			t.Errorf("CONTRIBUTING.md is missing current contract %q", required)
		}
	}
	for _, obsolete := range []string{
		"`schema.json` is a UI manifest, not a config contract",
		"`env.cache_*` is cross-request and **deliberately a shared flat keyspace**",
		"`env.meta_*` return\nempty",
	} {
		if strings.Contains(doc, obsolete) {
			t.Errorf("CONTRIBUTING.md retains obsolete contract %q", obsolete)
		}
	}
}
