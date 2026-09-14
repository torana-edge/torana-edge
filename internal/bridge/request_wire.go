package bridge

import (
	"bytes"
	"encoding/json"
	"io"

	pbjsontext "github.com/torana-edge/torana-plugin-sdk/pb/v1/jsontext"
)

func decodeClosed(raw []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return unsupported("multiple JSON values")
	}
	return nil
}

func closedMembers(raw []byte, allowed ...string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, unsupported("malformed request object")
	}
	for key := range obj {
		found := false
		for _, name := range allowed {
			if key == name {
				found = true
				break
			}
		}
		if !found {
			return nil, unsupported("unmodeled message or tool fields")
		}
	}
	return obj, nil
}

// The native adapters deliberately preserve unknown top-level fields. Some
// nested wire structs are narrower; check those before parsing so a bridge
// cannot approve a request after the decoder has discarded a constraint.
func validateRequestShape(p Protocol, raw []byte) error {
	// ParseRequest is also used directly by bridge integrations and tests, so
	// preserve the proxy's strict JSON boundary here: encoding/json alone would
	// accept duplicate decoded names and replace invalid Unicode before the
	// closed-shape checks can inspect it.
	if err := pbjsontext.Validate(raw); err != nil {
		return unsupported("malformed client request")
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || root == nil {
		return unsupported("malformed client request")
	}
	if p == GeminiCodeAssist {
		if _, ok := root["request"]; !ok {
			return unsupported("missing Code Assist request envelope")
		}
		inner := root["request"]
		root = nil
		if json.Unmarshal(inner, &root) != nil || root == nil {
			return unsupported("malformed Code Assist request envelope")
		}
	}
	var messages []json.RawMessage
	var tools []json.RawMessage
	switch p {
	case OpenAIChat, Anthropic:
		if _, ok := root["messages"]; !ok {
			return unsupported("missing messages")
		}
		if json.Unmarshal(root["messages"], &messages) != nil {
			return unsupported("malformed messages")
		}
	case OpenAIResponses:
		input, ok := root["input"]
		if !ok {
			return unsupported("missing Responses input; send the full conversation")
		}
		if len(input) == 0 {
			return unsupported("missing Responses input")
		}
		if input[0] != '"' && json.Unmarshal(input, &messages) != nil {
			return unsupported("malformed Responses input")
		}
	case Gemini, GeminiCodeAssist:
		if _, ok := root["contents"]; !ok {
			return unsupported("missing Gemini contents")
		}
		if json.Unmarshal(root["contents"], &messages) != nil {
			return unsupported("malformed Gemini contents")
		}
	}
	for _, rawMsg := range messages {
		switch p {
		case OpenAIChat:
			msg, err := closedMembers(rawMsg, "role", "content", "tool_calls", "tool_call_id", "name", "reasoning_content")
			if err != nil {
				return err
			}
			if name, present := msg["name"]; present {
				var role string
				_ = json.Unmarshal(msg["role"], &role)
				if role != "tool" || string(name) == "null" {
					return unsupported("named chat participants")
				}
			}
			if err = validateChatCalls(msg["tool_calls"]); err != nil {
				return err
			}
			if err = validateContentTextFields(msg["content"]); err != nil {
				return err
			}
		case Anthropic:
			msg, err := closedMembers(rawMsg, "role", "content")
			if err != nil {
				return err
			}
			if err = validateContentTextFields(msg["content"]); err != nil {
				return err
			}
		case OpenAIResponses:
			var msg map[string]json.RawMessage
			if json.Unmarshal(rawMsg, &msg) != nil || msg == nil {
				return unsupported("Responses input item")
			}
			var kind string
			_ = json.Unmarshal(msg["type"], &kind)
			if raw, present := msg["status"]; present {
				var status string
				if json.Unmarshal(raw, &status) != nil || status != "completed" {
					return unsupported("nonterminal Responses history items")
				}
			}
			switch kind {
			case "", "message":
				checked, err := closedMembers(rawMsg, "type", "role", "content", "id", "status")
				if err != nil {
					return err
				}
				if err = validateContentTextFields(checked["content"]); err != nil {
					return err
				}
			case "function_call":
				if _, err := closedMembers(rawMsg, "type", "id", "call_id", "name", "arguments", "status"); err != nil {
					return err
				}
			case "function_call_output":
				if _, err := closedMembers(rawMsg, "type", "id", "call_id", "output", "status"); err != nil {
					return err
				}
			default:
				return unsupported("opaque Responses input items")
			}
		case Gemini, GeminiCodeAssist:
			msg, err := closedMembers(rawMsg, "role", "parts")
			if err != nil {
				return err
			}
			if err = validateGeminiBridgeParts(msg["parts"]); err != nil {
				return err
			}
		}
	}
	if p == Gemini || p == GeminiCodeAssist {
		if raw, present := root["systemInstruction"]; present {
			system, err := closedMembers(raw, "role", "parts")
			if err != nil {
				return err
			}
			if roleRaw, present := system["role"]; present {
				var role string
				if json.Unmarshal(roleRaw, &role) != nil || role != "user" {
					return unsupported("Gemini system-instruction role")
				}
			}
			if err = validateGeminiBridgeParts(system["parts"]); err != nil {
				return err
			}
		}
	}
	if raw, ok := root["tools"]; ok {
		if json.Unmarshal(raw, &tools) != nil {
			return unsupported("malformed tools")
		}
		for _, rawTool := range tools {
			switch p {
			case OpenAIChat:
				obj, err := closedMembers(rawTool, "type", "function")
				if err != nil {
					return err
				}
				var kind string
				_ = json.Unmarshal(obj["type"], &kind)
				if kind != "function" {
					return unsupported("non-function tools")
				}
				if _, err = closedMembers(obj["function"], "name", "description", "parameters", "strict"); err != nil {
					return err
				}
			case OpenAIResponses:
				obj, err := closedMembers(rawTool, "type", "name", "description", "parameters", "strict")
				if err != nil {
					return err
				}
				var kind string
				_ = json.Unmarshal(obj["type"], &kind)
				if kind != "function" {
					return unsupported("non-function tools")
				}
			case Anthropic:
				if _, err := closedMembers(rawTool, "name", "description", "input_schema", "cache_control"); err != nil {
					return err
				}
			case Gemini, GeminiCodeAssist:
				obj, err := closedMembers(rawTool, "functionDeclarations")
				if err != nil {
					return err
				}
				var declarations []json.RawMessage
				if json.Unmarshal(obj["functionDeclarations"], &declarations) != nil || len(declarations) == 0 {
					return unsupported("non-function tools")
				}
				for _, declaration := range declarations {
					if _, err = closedMembers(declaration, "name", "description", "parameters", "parametersJsonSchema"); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func validateChatCalls(raw []byte) error {
	if raw == nil {
		return nil
	}
	var calls []json.RawMessage
	if json.Unmarshal(raw, &calls) != nil {
		return unsupported("malformed tool calls")
	}
	for _, call := range calls {
		obj, err := closedMembers(call, "id", "type", "function")
		if err != nil {
			return err
		}
		var kind string
		_ = json.Unmarshal(obj["type"], &kind)
		if kind != "function" {
			return unsupported("non-function tool calls")
		}
		if _, err = closedMembers(obj["function"], "name", "arguments"); err != nil {
			return err
		}
	}
	return nil
}

func validateContentTextFields(raw []byte) error {
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return unsupported("malformed content blocks")
	}
	for _, block := range blocks {
		var kind string
		_ = json.Unmarshal(block["type"], &kind)
		if kind == "text" || kind == "input_text" || kind == "output_text" {
			for key := range block {
				if key != "type" && key != "text" && key != "cache_control" {
					return unsupported("text annotations or provider content fields")
				}
			}
		}
	}
	return nil
}

func validateGeminiBridgeParts(raw []byte) error {
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return unsupported("Gemini content parts")
	}
	for _, part := range parts {
		if call, present := part["functionCall"]; present {
			if _, err := closedMembers(call, "id", "name", "args"); err != nil {
				return err
			}
		}
		if result, present := part["functionResponse"]; present {
			if _, err := closedMembers(result, "id", "name", "response", "parts", "willContinue", "scheduling"); err != nil {
				return err
			}
		}
	}
	return nil
}
