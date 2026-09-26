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

func TestConsentSummaryNestedLeavesAndIdentifiers(t *testing.T) {
	entry := namespaceEntry{Name: "router", ConfigSchema: json.RawMessage(`{"properties":{"private":{"writeOnly":true}}}`)}
	body, err := operationConsentSummary(entry, namespaceOperation{Source: "standard", ID: "_config.set"}, sealedOperationIntent{
		Before: json.RawMessage(`{"triggers":{"threshold":3},"ladders":[{"step":"small"}],"private":{"step":"old-private"}}`),
		After:  json.RawMessage(`{"triggers":{"threshold":5},"ladders":[{"step":"opus"}],"private":{"step":"new-private"}}`),
	})
	if err != nil || !strings.Contains(body, `"/triggers/threshold": 3 -> 5`) || !strings.Contains(body, `"/ladders/0/step": "small" -> "opus"`) || strings.Contains(body, "new-private") || strings.Contains(body, "old-private") {
		t.Fatalf("nested summary=%q %v", body, err)
	}
	if consentScalar("sk-should-not-display", nil, "step") != "[redacted]" {
		t.Fatal("credential-shaped identifier printed")
	}
}

func TestConsentIdentifiersDoNotExposeCommonTokens(t *testing.T) {
	for _, value := range []string{
		"AIzaSyD3xAmPl3K3y9876543210abcdefGHIJKL",
		"xoxb-123456789012-1234567890123-AbCdEfGhIjKlMnOpQrStUvWx",
		"4f9a2c7e1b8d6a3f5c0e9b7d2a1f8c6e",
		"sk_live_demo_not_real", "rk_live_demo_not_real", "glpat-demo-not-real", "hf_demo_not_real", "eyJdemo.not-real.signature",
		"abc123456789012", "AbCdEfGh1234IjKlMnOp", "AbCdEfGhIjKlMnOp",
	} {
		if got := consentScalar(value, nil, "value"); got != "[redacted]" {
			t.Errorf("credential-like identifier was not redacted")
		}
	}
	for _, value := range []string{"claude-opus-5", "gpt-5.4-mini", "step_1", "balanced"} {
		if got := consentScalar(value, nil, "value"); got == "[redacted]" {
			t.Errorf("human identifier %q was redacted", value)
		}
	}
	declared := "a-declared-enum-choice-longer-than-32"
	if consentScalar(declared, map[string]any{"enum": []any{declared}}, "mode") == "[redacted]" {
		t.Fatal("declared enum choice lost")
	}
	if consentScalar("balanced", map[string]any{"writeOnly": true, "enum": []any{"balanced"}}, "mode") != "[redacted]" {
		t.Fatal("enum bypassed sensitivity")
	}
}
