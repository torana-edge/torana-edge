package bridge

import (
	"encoding/json"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func projectExtensions(chat *engine.ChatRequest, from, to Protocol) error {
	ext := map[string]json.RawMessage{}
	if !chat.ProviderExtensions.IsAbsent() {
		if err := json.Unmarshal(chat.ProviderExtensions.Bytes(), &ext); err != nil {
			return unsupported("invalid provider extensions")
		}
	}
	if from == GeminiCodeAssist {
		inner := map[string]json.RawMessage{}
		if raw, ok := ext["request"]; ok && json.Unmarshal(raw, &inner) != nil {
			return unsupported("invalid Code Assist request envelope")
		}
		for name := range ext {
			if name != "request" && name != "project" {
				return unsupported("Code Assist envelope extensions")
			}
		}
		ext = inner
	}
	if from == Gemini || from == GeminiCodeAssist {
		if raw, ok := ext["generationConfig"]; ok {
			var gc map[string]json.RawMessage
			if json.Unmarshal(raw, &gc) != nil {
				return unsupported("invalid generation configuration")
			}
			if len(gc) > 0 {
				return unsupported("provider generation configuration extensions")
			}
			delete(ext, "generationConfig")
		}
	}
	dst := map[string]any{}
	for name, raw := range ext {
		switch name {
		case "instructions", "max_output_tokens", "temperature", "top_p":
			return unsupported("canonical request fields duplicated in provider extensions")
		case "max_completion_tokens":
			return unsupported("reasoning-inclusive completion token budgets")
		case "tool_choice":
			choice, err := parseToolChoice(from, raw)
			if err != nil {
				return err
			}
			if err = writeToolChoice(dst, to, choice); err != nil {
				return err
			}
		case "parallel_tool_calls":
			if from != OpenAIChat && from != OpenAIResponses {
				return unsupported("provider parallel tool configuration")
			}
			var parallel bool
			if string(raw) == "null" || json.Unmarshal(raw, &parallel) != nil {
				return unsupported("invalid parallel tool configuration")
			}
			switch to {
			case OpenAIChat, OpenAIResponses:
				dst[name] = parallel
			case Anthropic:
				choice, ok := dst["tool_choice"].(map[string]any)
				if !ok {
					choice = map[string]any{"type": "auto"}
				}
				choice["disable_parallel_tool_use"] = !parallel
				dst["tool_choice"] = choice
			default:
				if !parallel {
					return unsupported("serial tool calls on the upstream protocol")
				}
			}
		case "stream_options":
			if from != OpenAIChat {
				return unsupported("provider streaming configuration")
			}
			var options map[string]json.RawMessage
			if json.Unmarshal(raw, &options) != nil {
				return unsupported("invalid stream options")
			}
			for key, value := range options {
				var include bool
				if key != "include_usage" || string(value) == "null" || json.Unmarshal(value, &include) != nil {
					return unsupported("provider stream options")
				}
			}
		case "text", "output_config":
			// ExtractOutputFormat removes the portable constraint but deliberately
			// keeps an empty container to preserve native wire presence.
			var container map[string]json.RawMessage
			if json.Unmarshal(raw, &container) != nil || len(container) != 0 {
				return unsupported("provider output configuration extensions")
			}
		case "store", "background":
			if from != OpenAIResponses && from != OpenAIChat {
				return unsupported("provider request extensions")
			}
			var enabled bool
			if string(raw) == "null" || json.Unmarshal(raw, &enabled) != nil || enabled {
				return unsupported("server-managed response storage or background execution")
			}
		case "previous_response_id":
			if from != OpenAIResponses || string(raw) != "null" {
				return unsupported("server-managed response continuation; send the full conversation")
			}
		case "toolConfig":
			if from != Gemini && from != GeminiCodeAssist {
				return unsupported("provider tool configuration")
			}
			choice, err := parseGeminiToolChoice(raw)
			if err != nil {
				return err
			}
			if err = writeToolChoice(dst, to, choice); err != nil {
				return err
			}
		default:
			return unsupported("provider request extensions")
		}
	}
	// Parallel/tool-choice processing must be independent of Go map order.
	if raw, ok := ext["parallel_tool_calls"]; ok && to == Anthropic {
		var parallel bool
		_ = json.Unmarshal(raw, &parallel)
		choice, ok := dst["tool_choice"].(map[string]any)
		if !ok {
			choice = map[string]any{"type": "auto"}
		}
		choice["disable_parallel_tool_use"] = !parallel
		dst["tool_choice"] = choice
	}
	if !chat.ResponsesInputLayout.IsAbsent() {
		var layout []map[string]json.RawMessage
		if json.Unmarshal(chat.ResponsesInputLayout.Bytes(), &layout) != nil {
			return unsupported("Responses input layout")
		}
		for _, item := range layout {
			var kind string
			if json.Unmarshal(item["type"], &kind) != nil {
				return unsupported("Responses input layout")
			}
			switch kind {
			case "message", "function_call", "function_call_output":
			default:
				return unsupported("opaque Responses input items")
			}
		}
	}
	chat.ProviderExtensions = engine.OptionalJSONObject{}
	if len(dst) > 0 {
		raw, err := json.Marshal(dst)
		if err != nil {
			return err
		}
		chat.ProviderExtensions, err = engine.ParseOptionalJSONObject(raw)
		if err != nil {
			return err
		}
	}
	return nil
}

type toolChoice struct {
	mode, name string
	parallel   *bool
}

func parseToolChoice(from Protocol, raw []byte) (toolChoice, error) {
	var c toolChoice
	if from == OpenAIChat || from == OpenAIResponses {
		if json.Unmarshal(raw, &c.mode) == nil {
			switch c.mode {
			case "auto", "none", "required":
				return c, nil
			default:
				return c, unsupported("tool choice")
			}
		}
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return c, unsupported("tool choice")
	}
	var kind string
	_ = json.Unmarshal(obj["type"], &kind)
	switch from {
	case Anthropic:
		for key := range obj {
			if key != "type" && key != "name" && key != "disable_parallel_tool_use" {
				return c, unsupported("Anthropic tool choice extensions")
			}
		}
		switch kind {
		case "auto", "none":
			c.mode = kind
		case "any":
			c.mode = "required"
		case "tool":
			c.mode = "named"
		default:
			return c, unsupported("tool choice")
		}
		if raw, ok := obj["disable_parallel_tool_use"]; ok {
			var disabled bool
			if string(raw) == "null" || json.Unmarshal(raw, &disabled) != nil {
				return c, unsupported("parallel tool configuration")
			}
			enabled := !disabled
			c.parallel = &enabled
		}
		if c.mode == "named" {
			_ = json.Unmarshal(obj["name"], &c.name)
		}
	case OpenAIChat:
		if kind != "function" || len(obj) != 2 {
			return c, unsupported("tool choice")
		}
		var fn map[string]json.RawMessage
		if json.Unmarshal(obj["function"], &fn) != nil || len(fn) != 1 {
			return c, unsupported("tool choice")
		}
		_ = json.Unmarshal(fn["name"], &c.name)
		c.mode = "named"
	case OpenAIResponses:
		if kind != "function" || len(obj) != 2 {
			return c, unsupported("tool choice")
		}
		_ = json.Unmarshal(obj["name"], &c.name)
		c.mode = "named"
	default:
		return c, unsupported("tool choice")
	}
	if c.mode == "named" && c.name == "" {
		return c, unsupported("unnamed tool choice")
	}
	return c, nil
}

func parseGeminiToolChoice(raw []byte) (toolChoice, error) {
	var obj struct {
		FunctionCallingConfig struct {
			Mode                 string   `json:"mode"`
			AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
		} `json:"functionCallingConfig"`
	}
	if err := decodeClosed(raw, &obj); err != nil {
		return toolChoice{}, unsupported("Gemini tool configuration")
	}
	c := toolChoice{}
	switch obj.FunctionCallingConfig.Mode {
	case "AUTO":
		c.mode = "auto"
	case "NONE":
		c.mode = "none"
	case "ANY":
		c.mode = "required"
	default:
		return c, unsupported("Gemini tool calling mode")
	}
	names := obj.FunctionCallingConfig.AllowedFunctionNames
	if len(names) > 0 {
		if c.mode != "required" || len(names) != 1 || names[0] == "" {
			return c, unsupported("restricted tool-choice sets")
		}
		c.mode, c.name = "named", names[0]
	}
	return c, nil
}

func writeToolChoice(dst map[string]any, to Protocol, c toolChoice) error {
	switch to {
	case OpenAIChat, OpenAIResponses:
		if c.mode == "named" {
			if to == OpenAIChat {
				dst["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": c.name}}
			} else {
				dst["tool_choice"] = map[string]any{"type": "function", "name": c.name}
			}
		} else {
			dst["tool_choice"] = c.mode
		}
		if c.parallel != nil {
			dst["parallel_tool_calls"] = *c.parallel
		}
	case Anthropic:
		kind := c.mode
		if kind == "required" {
			kind = "any"
		}
		if kind == "named" {
			kind = "tool"
		}
		choice := map[string]any{"type": kind}
		if c.name != "" {
			choice["name"] = c.name
		}
		if c.parallel != nil {
			choice["disable_parallel_tool_use"] = !*c.parallel
		}
		dst["tool_choice"] = choice
	case Gemini, GeminiCodeAssist:
		if c.parallel != nil && !*c.parallel {
			return unsupported("serial tool calls on the upstream protocol")
		}
		mode := map[string]string{"auto": "AUTO", "none": "NONE", "required": "ANY", "named": "ANY"}[c.mode]
		fc := map[string]any{"mode": mode}
		if c.name != "" {
			fc["allowedFunctionNames"] = []string{c.name}
		}
		config := map[string]any{"functionCallingConfig": fc}
		if to == GeminiCodeAssist {
			dst["request"] = map[string]any{"toolConfig": config}
		} else {
			dst["toolConfig"] = config
		}
	default:
		return unsupported("upstream tool choice")
	}
	return nil
}

func validateProjectedToolDefinitionsAndChoice(chat *engine.ChatRequest, to Protocol) error {
	definitions := make(map[string]struct{}, len(chat.Tools))
	for i := range chat.Tools {
		name := chat.Tools[i].Name
		if _, duplicate := definitions[name]; duplicate {
			return unsupported("duplicate function tool definitions")
		}
		definitions[name] = struct{}{}
	}
	name, ok, err := projectedNamedToolChoice(chat, to)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := validateToolIdentity(to, name, ""); err != nil {
		return err
	}
	if _, exists := definitions[name]; !exists {
		return unsupported("named tool choice without a matching function definition")
	}
	return nil
}

func projectedNamedToolChoice(chat *engine.ChatRequest, to Protocol) (string, bool, error) {
	if chat.ProviderExtensions.IsAbsent() {
		return "", false, nil
	}
	members, _, err := chat.ProviderExtensions.DecodeObject()
	if err != nil {
		return "", false, unsupported("invalid projected provider extensions")
	}
	switch to {
	case OpenAIChat, OpenAIResponses, Anthropic:
		raw, ok := members["tool_choice"]
		if !ok {
			return "", false, nil
		}
		choice, err := parseToolChoice(to, raw)
		if err != nil {
			return "", false, err
		}
		return choice.name, choice.mode == "named", nil
	case Gemini:
		raw, ok := members["toolConfig"]
		if !ok {
			return "", false, nil
		}
		choice, err := parseGeminiToolChoice(raw)
		if err != nil {
			return "", false, err
		}
		return choice.name, choice.mode == "named", nil
	case GeminiCodeAssist:
		rawRequest, ok := members["request"]
		if !ok {
			return "", false, nil
		}
		var request map[string]json.RawMessage
		if json.Unmarshal(rawRequest, &request) != nil || request == nil {
			return "", false, unsupported("invalid projected Code Assist request envelope")
		}
		raw, ok := request["toolConfig"]
		if !ok {
			return "", false, nil
		}
		choice, err := parseGeminiToolChoice(raw)
		if err != nil {
			return "", false, err
		}
		return choice.name, choice.mode == "named", nil
	default:
		return "", false, unsupported("upstream tool choice")
	}
}
