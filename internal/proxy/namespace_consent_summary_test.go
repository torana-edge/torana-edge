package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConsentSummaryShowsActualChangeWithoutSecrets(t *testing.T) {
	entry := namespaceEntry{Name: "compactor", ConfigSchema: json.RawMessage(`{"properties":{"token":{"writeOnly":true},"threshold":{"type":"number"}}}`)}
	op := namespaceOperation{ID: "_config.set", Source: "standard"}
	body, err := operationConsentSummary(entry, op, sealedOperationIntent{Before: json.RawMessage(`{"threshold":5,"token":"old-secret","mode":"same"}`), After: json.RawMessage(`{"threshold":10,"token":"new-secret","mode":"same"}`)})
	if err != nil || !strings.Contains(body, `"/threshold": 5 -> 10`) || !strings.Contains(body, `"/token": [redacted] -> [redacted]`) || strings.Contains(body, "secret") || strings.Contains(body, "mode") {
		t.Fatalf("summary=%q %v", body, err)
	}
	op.ID = "_disable"
	body, err = operationConsentSummary(entry, op, sealedOperationIntent{})
	if err != nil || body != "Disable plugin compactor for all conversations." {
		t.Fatalf("summary=%q %v", body, err)
	}
}

func TestConsentSummaryDoesNotTruncateLargeApproval(t *testing.T) {
	input, _ := json.Marshal(map[string]any{strings.Repeat("x", 600): true})
	_, err := operationConsentSummary(namespaceEntry{Name: "logger"}, namespaceOperation{ID: "configure", Source: "plugin"}, sealedOperationIntent{Input: input})
	if err == nil {
		t.Fatal("truncated approval accepted")
	}
}
