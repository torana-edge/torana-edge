package middleware

import (
	"context"
	"net/http"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func TestIntentInjector(t *testing.T) {
	injector := NewIntentInjector()
	chat := &engine.ChatRequest{
		Tools: []engine.ToolDef{
			{
				Name: "bash",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
					},
					"required": []any{"command"},
				},
			},
		},
	}

	req, _ := http.NewRequest("POST", "/", nil)
	modChat, err := injector.BeforeRequest(context.Background(), req, chat)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tool := modChat.Tools[0]
	props, ok := tool.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties not a map")
	}
	if _, ok := props["_torana_extraction_intent"]; !ok {
		t.Error("missing _torana_extraction_intent in properties")
	}
	if _, ok := props["_torana_delegate_to_cheap_model"]; !ok {
		t.Error("missing _torana_delegate_to_cheap_model in properties")
	}

	reqs, ok := tool.Parameters["required"].([]any)
	if !ok {
		t.Fatal("required not a []any")
	}

	foundIntent := false
	foundDelegate := false
	for _, r := range reqs {
		if r == "_torana_extraction_intent" {
			foundIntent = true
		}
		if r == "_torana_delegate_to_cheap_model" {
			foundDelegate = true
		}
	}
	if !foundIntent {
		t.Error("missing _torana_extraction_intent in required")
	}
	if !foundDelegate {
		t.Error("missing _torana_delegate_to_cheap_model in required")
	}
}
