package main

import (
	"os"
	"strings"
	"testing"
)

// The local-model guide is a launch path, not background explanation. Keep it
// aligned with model-service bindings: the operator supplies the inference
// path, and intent improves compaction guidance without being a dependency.
func TestLocalModelGuideMatchesModelServiceContract(t *testing.T) {
	body, err := os.ReadFile("docs/LOCAL_MODELS.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(body)
	for _, required := range []string{
		"model-service approval separately names the root-relative",
		"bind that path to\n`/v1/chat/completions`",
		"The `intent` plugin is optional",
		`"order": ["compactor"]`,
	} {
		if !strings.Contains(doc, required) {
			t.Errorf("LOCAL_MODELS.md is missing current contract %q", required)
		}
	}
	for _, obsolete := range []string{
		"Torana appends\n`/v1/chat/completions`",
		"`intent` must run first",
	} {
		if strings.Contains(doc, obsolete) {
			t.Errorf("LOCAL_MODELS.md retains obsolete contract %q", obsolete)
		}
	}
}
