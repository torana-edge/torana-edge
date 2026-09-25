package effort

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/torana-edge/torana-edge/internal/provider"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func TestNativeEffortFiveShapesAndOutcomes(t *testing.T) {
	cases := []struct {
		shape, body string
		path        []string
		want        any
	}{
		{"anthropic", `{"output_config":{"format":{"type":"json_schema"}},"messages":[]}`, []string{"output_config", "effort"}, "high"},
		{"openai-chat", `{"messages":[]}`, []string{"reasoning_effort"}, "high"},
		{"openai-responses", `{"reasoning":{"summary":"auto"},"input":[]}`, []string{"reasoning", "effort"}, "high"},
		{"gemini", `{"generationConfig":{"maxOutputTokens":128,"thinkingConfig":{"includeThoughts":true}},"contents":[]}`, []string{"generationConfig", "thinkingConfig", "thinkingLevel"}, "HIGH"},
		{"gemini-codeassist", `{"request":{"generationConfig":{"thinkingConfig":{"includeThoughts":true}},"contents":[]}}`, []string{"request", "generationConfig", "thinkingConfig", "thinkingLevel"}, "HIGH"},
	}
	model := provider.ModelCapabilitiesConfig{Effort: &provider.ModelEffortConfig{
		Levels: []string{"low", "high"}, Gemini: map[string]provider.GeminiThinkingConfig{
			"low": {ThinkingLevel: "LOW"}, "high": {ThinkingLevel: "HIGH"},
		},
	}}
	for _, tc := range cases {
		t.Run(tc.shape, func(t *testing.T) {
			for _, outcome := range []struct {
				requested pb.Effort
				model     provider.ModelCapabilitiesConfig
				status    string
			}{
				{pb.Effort_EFFORT_HIGH, model, "applied"},
				{pb.Effort_EFFORT_MEDIUM, model, "clamped"},
				{pb.Effort_EFFORT_HIGH, provider.ModelCapabilitiesConfig{}, "omitted"},
				{pb.Effort_EFFORT_UNSPECIFIED, model, "unchanged"},
			} {
				got, level, status, err := Apply([]byte(tc.body), tc.shape, outcome.requested, outcome.model)
				if err != nil || status != outcome.status {
					t.Fatalf("effort %v: status %q, level %q, error %v", outcome.requested, status, level, err)
				}
				if status == "omitted" || status == "unchanged" {
					if !bytes.Equal(got, []byte(tc.body)) {
						t.Fatalf("%s changed request: %s", status, got)
					}
					continue
				}
				var object map[string]any
				if err := json.Unmarshal(got, &object); err != nil {
					t.Fatal(err)
				}
				var value any = object
				for _, key := range tc.path {
					value = value.(map[string]any)[key]
				}
				want := tc.want
				if status == "clamped" {
					want = "low"
					if tc.shape == "gemini" || tc.shape == "gemini-codeassist" {
						want = "LOW"
					}
				}
				if value != want {
					t.Fatalf("native effort = %v, want %v; body %s", value, want, got)
				}
			}
		})
	}
}

func TestNativeEffortMergesGeminiThinkingBudget(t *testing.T) {
	budget := 1024
	model := provider.ModelCapabilitiesConfig{Effort: &provider.ModelEffortConfig{
		Levels: []string{"low"}, Gemini: map[string]provider.GeminiThinkingConfig{"low": {ThinkingBudget: &budget}},
	}}
	got, _, status, err := Apply([]byte(`{"generationConfig":{"thinkingConfig":{"includeThoughts":true}},"contents":[]}`), "gemini", pb.Effort_EFFORT_LOW, model)
	if err != nil || status != "applied" || !bytes.Contains(got, []byte(`"includeThoughts":true`)) || !bytes.Contains(got, []byte(`"thinkingBudget":1024`)) {
		t.Fatalf("Gemini mapping did not merge: %s, %q, %v", got, status, err)
	}
}

func TestNativeEffortReplacesConflictingGeminiControl(t *testing.T) {
	model := provider.ModelCapabilitiesConfig{Effort: &provider.ModelEffortConfig{
		Levels: []string{"high"}, Gemini: map[string]provider.GeminiThinkingConfig{"high": {ThinkingLevel: "HIGH"}},
	}}
	got, _, status, err := Apply([]byte(`{"generationConfig":{"thinkingConfig":{"thinkingBudget":1024,"includeThoughts":true}}}`), "gemini", pb.Effort_EFFORT_HIGH, model)
	if err != nil || status != "applied" || bytes.Contains(got, []byte(`thinkingBudget`)) || !bytes.Contains(got, []byte(`"includeThoughts":true`)) {
		t.Fatalf("conflicting native control survived: %s, %q, %v", got, status, err)
	}
}
