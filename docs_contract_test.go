package main

import (
	"os"
	"strings"
	"testing"
)

// The agent-operation guide publishes a complete SDK handler. Keep the fenced
// example on the typed result API: the removed pointer-return/Handled API still
// looks plausible enough that a first-time author would copy it verbatim.
func TestAgentOperationGuideUsesCurrentHTTPResultAPI(t *testing.T) {
	body, err := os.ReadFile("docs/AGENT_CONTROL_PLANE.md")
	if err != nil {
		t.Fatal(err)
	}
	const marker = "Example Go SDK handler:\n\n```go\n"
	start := strings.Index(string(body), marker)
	if start < 0 {
		t.Fatal("agent-operation guide has no Go SDK handler example")
	}
	snippet := string(body)[start+len(marker):]
	if end := strings.Index(snippet, "\n```"); end >= 0 {
		snippet = snippet[:end]
	} else {
		t.Fatal("Go SDK handler example has no closing fence")
	}

	for _, required := range []string{
		"(sdk.HTTPResult, error)",
		"return sdk.PassHTTP(), nil",
		"return sdk.ServeHTTP(&pb.HttpResponse{",
	} {
		if !strings.Contains(snippet, required) {
			t.Errorf("Go SDK handler example is missing current API form %q", required)
		}
	}
	for _, removed := range []string{
		"(*pb.HttpResponse, error)",
		"Handled:",
		"return nil, nil",
	} {
		if strings.Contains(snippet, removed) {
			t.Errorf("Go SDK handler example retains removed API form %q", removed)
		}
	}
}
