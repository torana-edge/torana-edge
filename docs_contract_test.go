package main

import (
	"os"
	"strings"
	"testing"
)

func TestHarnessGuideSeparatesNativePassThroughFromBridges(t *testing.T) {
	body, err := os.ReadFile("docs/HARNESS_COMPATIBILITY.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(body)), " ")
	for _, required := range []string{
		"On a native route (no `bridge` configured)",
		"For a native provider route, non-inference traffic",
		"## Protocol bridges",
		"[bridge guide](PROTOCOL_BRIDGES.md)",
		"[CLI workflow](CLI.md#protocol-bridges)",
		"Under a mismatched bridge contract",
		"not emulated or forwarded; they return 400",
		"These preservation guarantees describe native routes",
		"not a claim that every live harness/backend combination has been tested",
		"A required endpoint returning the documented 400 is not a successful harness smoke test",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("harness guide is missing native/bridge boundary %q", required)
		}
	}
}

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
