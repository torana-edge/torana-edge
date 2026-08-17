package format_test

import (
	"net/http"
	"testing"

	"github.com/torana-edge/torana-edge/internal/format"
	_ "github.com/torana-edge/torana-edge/internal/format/anthropic"
	_ "github.com/torana-edge/torana-edge/internal/format/bedrock"
	_ "github.com/torana-edge/torana-edge/internal/format/gemini"
	_ "github.com/torana-edge/torana-edge/internal/format/openai"
)

func TestInferenceEndpointClassification(t *testing.T) {
	t.Parallel()

	rows := []struct {
		format string
		method string
		path   string
		want   bool
	}{
		{"openai", http.MethodPost, "/v1/chat/completions", true},
		{"openai", http.MethodPost, "/deployments/a/chat/completions", true},
		{"openai", http.MethodPost, "/v1/responses", true},
		{"openai", http.MethodPost, "/v1/responses/", true},
		{"openai", http.MethodGet, "/v1/chat/completions", false},
		{"openai", http.MethodPost, "/v1/models", false},
		{"openai", http.MethodPost, "/status/chat/completions/detail", false},

		{"anthropic", http.MethodPost, "/v1/messages", true},
		{"anthropic", http.MethodPost, "/messages/", true},
		{"anthropic", http.MethodGet, "/v1/messages", false},
		{"anthropic", http.MethodPost, "/v1/messages/count_tokens", false},
		{"anthropic", http.MethodPost, "/api/oauth/usage", false},

		{"gemini", http.MethodPost, "/v1beta/models/gemini:generateContent", true},
		{"gemini", http.MethodPost, "/v1beta/models/gemini:streamGenerateContent", true},
		{"gemini-codeassist", http.MethodPost, "/v1internal:generateContent", true},
		{"gemini-codeassist", http.MethodPost, "/v1internal:streamGenerateContent", true},
		{"gemini", http.MethodGet, "/v1beta/models/gemini:generateContent", false},
		{"gemini", http.MethodPost, "/v1beta/models", false},
		{"gemini", http.MethodPost, "/status:generateContent/detail", false},

		{"bedrock", http.MethodPost, "/model/claude/converse", true},
		{"bedrock", http.MethodPost, "/model/claude/converse-stream", true},
		{"bedrock", http.MethodPost, "/model/claude/invoke", true},
		{"bedrock", http.MethodPost, "/model/claude/invoke-with-response-stream", true},
		{"bedrock", http.MethodGet, "/model/claude/converse", false},
		{"bedrock", http.MethodPost, "/foundation-models", false},
		{"bedrock", http.MethodPost, "/status/model/x/invoke/detail", false},
	}

	for _, row := range rows {
		row := row
		t.Run(row.format+"_"+row.method+"_"+row.path, func(t *testing.T) {
			got := format.Lookup(row.format).HandlesInference(row.method, row.path)
			if got != row.want {
				t.Fatalf("HandlesInference(%q, %q) = %v, want %v", row.method, row.path, got, row.want)
			}
		})
	}
}

func TestFormatWithoutEndpointPolicyDeclinesInferenceOwnership(t *testing.T) {
	f := &format.Format{Name: "custom"}
	if f.HandlesInference(http.MethodPost, "/v1/chat/completions") {
		t.Fatal("format without an explicit endpoint policy claimed inference traffic")
	}
}
