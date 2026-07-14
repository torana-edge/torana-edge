package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

const mutationsKey = "mutations"

// activeToolKey returns the meta key for tracking the current tool call ID by index.
func activeToolKey(index int32) string { return fmt.Sprintf("tool:%d", index) }

// fragmentKey returns the meta key for accumulating tool call argument fragments.
func fragmentKey(toolCallID string) string { return "frag:" + toolCallID }

func init() {
	// ── Request side: KV-array schema conversion ──────────────────────
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		if len(req.Tools) == 0 {
			return nil, nil
		}

		modified := false
		registry := make(map[string][]string)

		for _, tool := range req.Tools {
			if len(tool.ParametersJson) == 0 {
				continue
			}
			var params map[string]any
			if err := json.Unmarshal(tool.ParametersJson, &params); err != nil {
				continue
			}

			mutations := translateSchema(tool.Name, params, "")
			if len(mutations) > 0 {
				registry[tool.Name] = mutations
			}

			newJSON, err := json.Marshal(params)
			if err != nil {
				continue
			}
			if string(newJSON) != string(tool.ParametersJson) {
				tool.ParametersJson = newJSON
				modified = true
			}
		}

		if len(registry) > 0 {
			regJSON, _ := json.Marshal(registry)
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"%s","value":%s}`, mutationsKey, string(regJSON)))
		}

		if !modified {
			return nil, nil
		}
		return req, nil
	})

	// ── Response side: reverse KV arrays ────────────────────────────
	sdk.OnStreamChunk(func(ctx context.Context, chunk *pb.StreamEvent) (*pb.StreamEvent, error) {
		// Track the current tool call by index (ToolCallStart is the only event with ID).
		if ts := chunk.GetToolCallStart(); ts != nil {
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"%s","value":%q}`, activeToolKey(ts.Index), ts.Id))
			return nil, nil // pass through
		}

		// Accumulate ToolCallDelta fragments.
		if td := chunk.GetToolCallDelta(); td != nil {
			toolID, _ := sdk.HostCall("env.meta_get", activeToolKey(td.Index))
			if toolID == "" {
				return nil, nil
			}
			key := fragmentKey(toolID)
			prev, _ := sdk.HostCall("env.meta_get", key)
			accumulated := prev + td.ArgumentsDelta
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"%s","value":%q}`, key, accumulated))

			// For non-streaming JSON responses (full args in single delta),
			// reverse in-place and return immediately.
			args := td.ArgumentsDelta
			if strings.HasPrefix(strings.TrimSpace(args), "{") && strings.HasSuffix(strings.TrimSpace(args), "}") {
				regRaw, _ := sdk.HostCall("env.meta_get", mutationsKey)
				var registry map[string][]string
				if regRaw != "" {
					json.Unmarshal([]byte(regRaw), &registry)
				}
				reversed, _ := reverseTranslate("", args, registry)
				if reversed != args {
					sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"%s","value":%q}`, key, reversed))
					return &pb.StreamEvent{
						Event: &pb.StreamEvent_ToolCallDelta{
							ToolCallDelta: &pb.ToolCallDelta{
								Index:          td.Index,
								ArgumentsDelta: reversed,
							},
						},
					}, nil
				}
			}
			// Fragment — suppress, buffer until ToolCallEnd.
			return nil, nil
		}

		// On ToolCallEnd for streaming: emit reversed args from buffered fragments.
		if te := chunk.GetToolCallEnd(); te != nil {
			toolID, _ := sdk.HostCall("env.meta_get", activeToolKey(te.Index))
			if toolID == "" {
				return nil, nil
			}
			key := fragmentKey(toolID)
			fullArgs, _ := sdk.HostCall("env.meta_get", key)
			// Clean up.
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"%s","value":""}`, key))
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"%s","value":""}`, activeToolKey(te.Index)))

			if fullArgs == "" || !strings.HasPrefix(fullArgs, "{") {
				return nil, nil
			}

			regRaw, _ := sdk.HostCall("env.meta_get", mutationsKey)
			var registry map[string][]string
			if regRaw != "" {
				json.Unmarshal([]byte(regRaw), &registry)
			}
			reversed, _ := reverseTranslate("", fullArgs, registry)

			if reversed == fullArgs {
				return nil, nil
			}

			// Emit reversed delta. Server forwards ToolCallEnd after this.
			return &pb.StreamEvent{
				Event: &pb.StreamEvent_ToolCallDelta{
					ToolCallDelta: &pb.ToolCallDelta{
						Index:          te.Index,
						ArgumentsDelta: reversed,
					},
				},
			}, nil
		}

		return nil, nil
	})
}

// ==========================================================================
// Schema translation
// ==========================================================================

func translateSchema(toolName string, schema map[string]any, path string) []string {
	var mutations []string

	props, hasProps := schema["properties"].(map[string]any)
	_, hasExplicitAP := schema["additionalProperties"]
	schemaType, _ := schema["type"].(string)

	if schemaType == "object" && !hasProps && !hasExplicitAP {
		convertToKVArray(schema, "string")
		mutations = append(mutations, path)
		return mutations
	}

	schema["additionalProperties"] = false
	if !hasProps {
		return mutations
	}

	for propName, propVal := range props {
		propSchema, ok := propVal.(map[string]any)
		if !ok {
			continue
		}
		currentPath := joinPath(path, propName)
		propType, _ := propSchema["type"].(string)
		_, propHasProps := propSchema["properties"].(map[string]any)
		_, propHasAP := propSchema["additionalProperties"]

		if propType == "object" && !propHasProps && !propHasAP {
			convertToKVArray(propSchema, "string")
			mutations = append(mutations, currentPath)
			continue
		}
		if hasAdditionalProperties(propSchema) {
			valueType := extractAdditionalPropertiesType(propSchema)
			convertToKVArray(propSchema, valueType)
			mutations = append(mutations, currentPath)
			continue
		}
		propSchema["additionalProperties"] = false
		switch propType {
		case "object":
			mutations = append(mutations, translateSchema(toolName, propSchema, currentPath)...)
		case "array":
			if items, ok := propSchema["items"].(map[string]any); ok {
				if itemType, _ := items["type"].(string); itemType == "object" {
					mutations = append(mutations, translateSchema(toolName, items, currentPath+"[]")...)
				}
			}
		}
	}
	return mutations
}

func hasAdditionalProperties(schema map[string]any) bool {
	ap, exists := schema["additionalProperties"]
	if !exists {
		return false
	}
	switch v := ap.(type) {
	case bool:
		return v
	case map[string]any:
		return true
	}
	return false
}

func extractAdditionalPropertiesType(schema map[string]any) string {
	if ap, ok := schema["additionalProperties"].(map[string]any); ok {
		if t, ok := ap["type"].(string); ok {
			return t
		}
	}
	return "string"
}

func convertToKVArray(schema map[string]any, valueType string) {
	desc := ""
	if d, ok := schema["description"].(string); ok {
		desc = d
	}
	for k := range schema {
		delete(schema, k)
	}
	schema["type"] = "array"
	schema["description"] = desc + " (as key-value pairs: [{\"key\": \"...\", \"value\": \"...\"}])"
	schema["items"] = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"key":   map[string]any{"type": "string", "description": "the key name"},
			"value": map[string]any{"type": valueType, "description": "the value"},
		},
		"additionalProperties": false,
		"required":             []any{"key", "value"},
	}
}

// ==========================================================================
// Reverse translation
// ==========================================================================

func reverseTranslate(toolName string, argsJSON string, registry map[string][]string) (string, string) {
	if argsJSON == "" || argsJSON == "{}" {
		return argsJSON, ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return argsJSON, ""
	}

	reversed := false
	if paths, ok := registry[toolName]; ok && toolName != "" {
		for _, path := range paths {
			reverseKVArrayAtPath(args, path)
		}
		reversed = true
	}
	if !reversed {
		args = heuristicKVReversal(args)
	}

	b, err := json.Marshal(args)
	if err != nil {
		return argsJSON, ""
	}
	return string(b), ""
}

func reverseKVArrayAtPath(args map[string]any, path string) {
	reverseAtPath(args, strings.Split(path, "."))
}

func reverseAtPath(obj map[string]any, parts []string) {
	if len(parts) == 0 {
		return
	}
	current := parts[0]
	rest := parts[1:]

	if strings.HasSuffix(current, "[]") {
		fieldName := strings.TrimSuffix(current, "[]")
		arr, ok := obj[fieldName].([]any)
		if !ok {
			return
		}
		if len(rest) == 0 {
			for i, item := range arr {
				if itemMap, ok := item.(map[string]any); ok {
					arr[i] = reverseKVObject(itemMap)
				}
			}
		} else {
			for _, item := range arr {
				if itemMap, ok := item.(map[string]any); ok {
					reverseAtPath(itemMap, rest)
				}
			}
		}
		return
	}

	if len(rest) == 0 {
		if val, ok := obj[current]; ok {
			if arr, ok := val.([]any); ok {
				obj[current] = reverseKVArray(arr)
			}
		}
		return
	}

	if nested, ok := obj[current].(map[string]any); ok {
		reverseAtPath(nested, rest)
	}
}

func reverseKVArray(arr []any) map[string]any {
	result := make(map[string]any, len(arr))
	for _, item := range arr {
		kv, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key, _ := kv["key"].(string)
		if key == "" {
			continue
		}
		if val, exists := kv["value"]; exists {
			result[key] = val
		}
	}
	return result
}

func reverseKVObject(obj map[string]any) map[string]any {
	for k, v := range obj {
		if arr, ok := v.([]any); ok && isKVArray(arr) {
			obj[k] = reverseKVArray(arr)
		}
	}
	return obj
}

func heuristicKVReversal(args map[string]any) map[string]any {
	for k, v := range args {
		switch val := v.(type) {
		case map[string]any:
			args[k] = heuristicKVReversal(val)
		case []any:
			if isKVArray(val) {
				args[k] = reverseKVArray(val)
			} else {
				for i, item := range val {
					if m, ok := item.(map[string]any); ok {
						val[i] = heuristicKVReversal(m)
					}
				}
			}
		}
	}
	return args
}

func isKVArray(arr []any) bool {
	if len(arr) == 0 {
		return false
	}
	if m, ok := arr[0].(map[string]any); ok {
		_, hasKey := m["key"]
		_, hasValue := m["value"]
		return hasKey && hasValue
	}
	return false
}

func joinPath(base, segment string) string {
	if base == "" {
		return segment
	}
	return base + "." + segment
}
