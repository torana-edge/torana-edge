package proxy

import (
	"encoding/json"
	"fmt"
)

// validateModelServiceResultDomain verifies that every user-visible block in
// the selected provider result has a canonical ModelCompleteResult arm. The
// normal response pipeline may preserve provider-only fields in the original
// body; model-service callers receive a new typed value, so silently skipping
// an unsupported block here would turn a refusal, image, or custom tool call
// into a successful partial (sometimes empty) answer.
func validateModelServiceResultDomain(format string, body map[string]any) error {
	switch format {
	case "openai":
		if output, present := body["output"]; present {
			items, ok := output.([]any)
			if !ok {
				return fmt.Errorf("openai output is not an array")
			}
			return validateOpenAIResponsesResult(items)
		}
		return validateOpenAIChatResult(body)
	case "anthropic":
		return validateAnthropicResult(body)
	case "gemini", "gemini-codeassist":
		if inner, ok := body["response"].(map[string]any); ok {
			body = inner
		}
		return validateGeminiResult(body)
	default:
		return fmt.Errorf("unsupported provider format %q", format)
	}
}

func validateOpenAIChatResult(body map[string]any) error {
	choices, ok := body["choices"].([]any)
	if !ok || len(choices) == 0 {
		return fmt.Errorf("openai choices are absent")
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return fmt.Errorf("openai selected choice is not an object")
	}
	message, ok := choice["message"].(map[string]any)
	if !ok {
		return fmt.Errorf("openai selected message is not an object")
	}
	if content, present := message["content"]; present && content != nil {
		if _, ok := content.(string); !ok {
			return fmt.Errorf("openai message content is not text")
		}
	}
	for _, field := range []string{"refusal", "audio", "function_call"} {
		if value, present := message[field]; present && value != nil && value != "" {
			return fmt.Errorf("openai message contains unsupported %s output", field)
		}
	}
	if calls, present := message["tool_calls"]; present && calls != nil {
		items, ok := calls.([]any)
		if !ok {
			return fmt.Errorf("openai tool_calls is not an array")
		}
		for i, item := range items {
			call, ok := item.(map[string]any)
			if !ok || asString(call["type"]) != "function" {
				return fmt.Errorf("openai tool call %d is not a function call", i)
			}
			if err := validateStringArgsFunction(call["function"]); err != nil {
				return fmt.Errorf("openai tool call %d: %w", i, err)
			}
		}
	}
	return nil
}

func validateOpenAIResponsesResult(items []any) error {
	for i, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("openai output item %d is not an object", i)
		}
		switch asString(object["type"]) {
		case "reasoning": // provider reasoning metadata, not a user-visible answer
			continue
		case "message":
			parts, ok := object["content"].([]any)
			if !ok {
				return fmt.Errorf("openai output message %d has invalid content", i)
			}
			for j, part := range parts {
				p, ok := part.(map[string]any)
				if !ok || asString(p["type"]) != "output_text" {
					return fmt.Errorf("openai output message %d part %d is not text", i, j)
				}
				if _, ok := p["text"].(string); !ok {
					return fmt.Errorf("openai output message %d part %d has invalid text", i, j)
				}
			}
		case "function_call":
			if err := validateStringArgsCall(object); err != nil {
				return fmt.Errorf("openai function call %d: %w", i, err)
			}
		default:
			return fmt.Errorf("openai output item %d has unsupported type %q", i, asString(object["type"]))
		}
	}
	return nil
}

func validateAnthropicResult(body map[string]any) error {
	blocks, ok := body["content"].([]any)
	if !ok {
		return fmt.Errorf("anthropic content is not an array")
	}
	for i, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("anthropic content block %d is not an object", i)
		}
		switch asString(block["type"]) {
		case "thinking", "redacted_thinking":
			continue
		case "text":
			if _, ok := block["text"].(string); !ok {
				return fmt.Errorf("anthropic text block %d has invalid text", i)
			}
		case "tool_use":
			if asString(block["name"]) == "" || !isJSONObject(block["input"]) {
				return fmt.Errorf("anthropic tool block %d is malformed", i)
			}
		default:
			return fmt.Errorf("anthropic content block %d has unsupported type %q", i, asString(block["type"]))
		}
	}
	return nil
}

func validateGeminiResult(body map[string]any) error {
	candidates, ok := body["candidates"].([]any)
	if !ok || len(candidates) == 0 {
		return fmt.Errorf("gemini candidates are absent")
	}
	candidate, ok := candidates[0].(map[string]any)
	if !ok {
		return fmt.Errorf("gemini selected candidate is not an object")
	}
	content, ok := candidate["content"].(map[string]any)
	if !ok {
		return fmt.Errorf("gemini selected content is not an object")
	}
	parts, ok := content["parts"].([]any)
	if !ok {
		return fmt.Errorf("gemini selected parts are not an array")
	}
	for i, item := range parts {
		part, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("gemini part %d is not an object", i)
		}
		for key := range part {
			switch key {
			case "text", "functionCall", "thought", "thoughtSignature", "partMetadata":
			default:
				return fmt.Errorf("gemini part %d has unsupported output field %q", i, key)
			}
		}
		_, hasText := part["text"]
		function, hasFunction := part["functionCall"]
		if thought, _ := part["thought"].(bool); thought && hasText {
			// Reasoning text is provider metadata, not assistant output. Delete
			// only the text member; retaining the part keeps raw tool-argument
			// array indices stable for extractResponse.
			delete(part, "text")
			hasText = false
		}
		if !hasText && !hasFunction {
			if thought, _ := part["thought"].(bool); thought {
				continue
			}
			return fmt.Errorf("gemini part %d has no supported output arm", i)
		}
		if hasText == hasFunction {
			return fmt.Errorf("gemini part %d has no single supported output arm", i)
		}
		if hasText {
			if _, ok := part["text"].(string); !ok {
				return fmt.Errorf("gemini part %d has invalid text", i)
			}
			continue
		}
		call, ok := function.(map[string]any)
		if !ok || asString(call["name"]) == "" || !isJSONObject(call["args"]) {
			return fmt.Errorf("gemini function call %d is malformed", i)
		}
	}
	return nil
}

func validateStringArgsFunction(value any) error {
	fn, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("function is not an object")
	}
	return validateStringArgsCall(fn)
}

func validateStringArgsCall(call map[string]any) error {
	if asString(call["name"]) == "" {
		return fmt.Errorf("function name is absent")
	}
	args, ok := call["arguments"].(string)
	if !ok {
		return fmt.Errorf("function arguments are not a string")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &object) != nil || object == nil {
		return fmt.Errorf("function arguments are not a JSON object")
	}
	return nil
}

func isJSONObject(value any) bool {
	_, ok := value.(map[string]any)
	return ok
}
