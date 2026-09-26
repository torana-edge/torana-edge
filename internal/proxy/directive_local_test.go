package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/torana-edge/torana-edge/internal/annotate"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/secret"
)

func TestRenderDirectiveReplyCarriesSignedReplayMarker(t *testing.T) {
	signer, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{secrets: signer}
	for _, tc := range []struct {
		name, format string
		variant      engine.OpenAIVariant
	}{
		{"anthropic", "anthropic", engine.OpenAIChat},
		{"chat", "openai", engine.OpenAIChat},
		{"responses", "openai", engine.OpenAIResponses},
		{"gemini", "gemini", engine.OpenAIChat},
		{"codeassist", "gemini-codeassist", engine.OpenAIChat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chat := &engine.ChatRequest{Model: "model", OpenAIVariant: tc.variant}
			rendered, err := s.renderDirectiveReply(context.Background(), format.Lookup(tc.format), chat, "conversation", []byte(`{"previous_response_id":"resp_real"}`), "Suggestion accepted")
			if err != nil {
				t.Fatal(err)
			}
			if rendered.Status != 200 || !bytes.Contains(rendered.Body, []byte("torana:begin:local_")) {
				t.Fatalf("local reply lacks signed marker: %s", rendered.Body)
			}
			if tc.name == "responses" {
				var body map[string]any
				if err := json.Unmarshal(rendered.Body, &body); err != nil {
					t.Fatal(err)
				}
				providerID, recognized, err := annotate.DecodeLocalResponseID(signer, body["id"].(string))
				if err != nil || !recognized || providerID != "resp_real" {
					t.Fatalf("response ID did not carry provider parent: %q, %v, %v", providerID, recognized, err)
				}
			}
		})
	}
}
