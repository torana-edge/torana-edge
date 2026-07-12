package main

import (
	"encoding/json"

	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() { sdk.Init() }

// alloc and dealloc must live in the main package for //go:export to work.
//
//go:export alloc
func alloc(size uint32) uint32 { return sdk.Alloc(size) }

//go:export dealloc
func dealloc(ptr, size uint32) {}

// on_chat_request injects the "i" field into every tool schema and
// converts additionalProperties maps to KV arrays for strict-mode compliance.
//
//go:export on_chat_request
func on_chat_request(ptr, size uint32) uint64 {
	input := sdk.GetBytes(ptr, size)

	var msg struct {
		Chat  string `json:"chat"`
		Tools []struct {
			Name       string         `json:"name"`
			Parameters map[string]any `json:"parameters"`
		} `json:"tools"`
	}
	if json.Unmarshal(input, &msg) != nil {
		return 0
	}

	modified := false
	for i := range msg.Tools {
		t := &msg.Tools[i]
		if t.Parameters == nil {
			continue
		}
		props, ok := t.Parameters["properties"].(map[string]any)
		if !ok {
			continue
		}
		if _, hasI := props["i"]; !hasI {
			props["i"] = map[string]any{
				"type":        "string",
				"description": "what you intend to accomplish",
			}
			if required, ok := t.Parameters["required"].([]any); ok {
				t.Parameters["required"] = append(required, "i")
			}
			modified = true
		}
		t.Parameters = translateSchema(t.Parameters)
	}

	if !modified {
		return 0
	}
	return sdk.WriteResult(msg)
}

func translateSchema(schema map[string]any) map[string]any {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return schema
	}
	for key, val := range props {
		propSchema, ok := val.(map[string]any)
		if !ok {
			continue
		}
		if tp, _ := propSchema["type"].(string); tp != "object" {
			continue
		}
		ap, hasAP := propSchema["additionalProperties"]
		_, hasProps := propSchema["properties"].(map[string]any)
		if (!hasProps && !hasAP) || (hasAP && ap != false) {
			props[key] = map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"key":   map[string]any{"type": "string"},
						"value": map[string]any{"type": "string"},
					},
					"required":             []any{"key", "value"},
					"additionalProperties": false,
				},
			}
		} else {
			propSchema["additionalProperties"] = false
			props[key] = translateSchema(propSchema)
		}
	}
	return schema
}
