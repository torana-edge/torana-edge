package middleware

import (
	"context"
	"log"
	"net/http"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// IntentInjector implements V1 schema-level intent extraction.
// It modifies the input schema of all tools to require the LLM to
// declare its intent before the tool runs.
type IntentInjector struct{}

// NewIntentInjector creates a new IntentInjector middleware.
func NewIntentInjector() *IntentInjector {
	return &IntentInjector{}
}

func (i *IntentInjector) Name() string { return "intent-injector" }

// BeforeRequest injects _torana_extraction_intent into tool parameters.
func (i *IntentInjector) BeforeRequest(ctx context.Context, req *http.Request, chat *engine.ChatRequest) (*engine.ChatRequest, error) {
	if chat == nil || len(chat.Tools) == 0 {
		return chat, nil
	}

	for idx := range chat.Tools {
		tool := &chat.Tools[idx]
		if tool.Parameters == nil {
			tool.Parameters = make(map[string]any)
		}

		if tool.Parameters["type"] == nil {
			tool.Parameters["type"] = "object"
		}

		props, ok := tool.Parameters["properties"].(map[string]any)
		if !ok {
			props = make(map[string]any)
			tool.Parameters["properties"] = props
		}

		// Force strict schema mode so DeepSeek and OpenAI are mathematically required to output intent.
		tool.Strict = true
		tool.Parameters["additionalProperties"] = false

		props["_torana_extraction_intent"] = map[string]any{
			"type":        "string",
			"description": "CRITICAL: specify what you are looking for in the tool result to help the proxy compact it. If you omit this, you will fail the task.",
		}
		props["_torana_delegate_to_cheap_model"] = map[string]any{
			"type":        "boolean",
			"description": "CRITICAL: set to true if you expect this tool to return massive output (e.g. recursive grep, big files) and you want a cheaper model to summarize the output based on your extraction intent.",
		}

		// Handle required slice which could be unmarshaled as []any or missing
		requiredField := tool.Parameters["required"]
		var reqArr []any
		switch v := requiredField.(type) {
		case []any:
			reqArr = v
		case []string:
			for _, s := range v {
				reqArr = append(reqArr, s)
			}
		default:
			reqArr = make([]any, 0)
		}

		// In strict mode, every single property MUST be required.
		// Re-build the required array from the keys of props.
		reqArr = make([]any, 0, len(props))
		for k := range props {
			reqArr = append(reqArr, k)
		}
		tool.Parameters["required"] = reqArr
	}

	log.Printf("[intent-injector] injected into %d tools", len(chat.Tools))
	return chat, nil
}
