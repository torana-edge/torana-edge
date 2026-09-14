package bridge

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
)

// MarshalRequest owns the protocol bridge's final wire projection. Public
// Gemini has a JSON Schema field distinct from its OpenAPI-like Schema type.
// The native adapter retains native topology; this explicit boundary chooses
// the destination representation for schemas already normalized into the IR.
func MarshalRequest(p Protocol, chat *engine.ChatRequest) ([]byte, error) {
	f := format.Lookup(p.Format())
	if f == nil {
		return nil, unsupported("unregistered upstream protocol")
	}
	raw, err := f.Request.Marshal(chat)
	if err != nil {
		return nil, err
	}
	if p != Gemini && p != GeminiCodeAssist {
		return raw, nil
	}
	return rewriteGeminiSchemas(raw, p, func(decl map[string]json.RawMessage) error {
		schema, ok := decl["parameters"]
		if !ok {
			return nil
		}
		if p == Gemini {
			decl["parametersJsonSchema"] = schema
			delete(decl, "parameters")
		} else {
			converted, err := convertGeminiSchema(schema, false)
			if err != nil {
				return err
			}
			decl["parameters"] = converted
		}
		return nil
	})
}

func normalizeGeminiRequestSchemas(raw []byte, p Protocol) ([]byte, error) {
	return rewriteGeminiSchemas(raw, p, func(decl map[string]json.RawMessage) error {
		schema, legacy := decl["parameters"]
		canonical, jsonSchema := decl["parametersJsonSchema"]
		if legacy && jsonSchema {
			return unsupported("conflicting Gemini function schemas")
		}
		if jsonSchema {
			if _, err := engine.ParseRequiredJSONObject(canonical); err != nil {
				return unsupported("invalid function JSON Schema")
			}
			decl["parameters"] = canonical
			delete(decl, "parametersJsonSchema")
			return nil
		}
		if legacy {
			converted, err := convertGeminiSchema(schema, true)
			if err != nil {
				return err
			}
			decl["parameters"] = converted
		}
		return nil
	})
}

func rewriteGeminiSchemas(raw []byte, p Protocol, rewrite func(map[string]json.RawMessage) error) ([]byte, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || root == nil {
		return nil, unsupported("Gemini request object")
	}
	inner := root
	if p == GeminiCodeAssist {
		inner = nil
		if json.Unmarshal(root["request"], &inner) != nil || inner == nil {
			return nil, unsupported("Code Assist request envelope")
		}
	}
	toolsRaw, present := inner["tools"]
	if !present {
		return raw, nil
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(toolsRaw, &tools) != nil {
		return nil, unsupported("Gemini tools")
	}
	for _, tool := range tools {
		var declarations []map[string]json.RawMessage
		if json.Unmarshal(tool["functionDeclarations"], &declarations) != nil {
			return nil, unsupported("Gemini function declarations")
		}
		for _, declaration := range declarations {
			if err := rewrite(declaration); err != nil {
				return nil, err
			}
		}
		encoded, err := json.Marshal(declarations)
		if err != nil {
			return nil, err
		}
		tool["functionDeclarations"] = encoded
	}
	encoded, err := json.Marshal(tools)
	if err != nil {
		return nil, err
	}
	inner["tools"] = encoded
	if p == GeminiCodeAssist {
		encoded, err = json.Marshal(inner)
		if err != nil {
			return nil, err
		}
		root["request"] = encoded
	}
	return json.Marshal(root)
}

// convertGeminiSchema supports the documented common Schema subset without
// changing validation constraints. Unknown keywords are refused; defaults,
// numbers and user property names remain RawMessage lexemes. Property ordering
// and provider-only schema features need an explicit mapping before support.
func convertGeminiSchema(raw []byte, fromGemini bool) ([]byte, error) {
	return convertGeminiSchemaDepth(raw, fromGemini, 0)
}

func convertGeminiSchemaDepth(raw []byte, fromGemini bool, depth int) ([]byte, error) {
	if depth > 64 {
		return nil, unsupported("excessively nested function schemas")
	}
	obj, err := engine.ParseRequiredJSONObject(raw)
	if err != nil {
		return nil, unsupported("function schema object")
	}
	members, _, err := obj.DecodeObject()
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	for key, value := range members {
		switch key {
		case "type":
			var typ string
			if json.Unmarshal(value, &typ) != nil {
				return nil, unsupported("union schema types on the upstream protocol")
			}
			normalized := strings.ToLower(typ)
			switch normalized {
			case "object", "array", "string", "integer", "number", "boolean", "null":
			default:
				return nil, unsupported("function schema type")
			}
			if !fromGemini {
				normalized = strings.ToUpper(normalized)
			}
			out[key], _ = json.Marshal(normalized)
		case "properties":
			var properties map[string]json.RawMessage
			if json.Unmarshal(value, &properties) != nil || properties == nil {
				return nil, unsupported("function schema properties")
			}
			for name, child := range properties {
				converted, err := convertGeminiSchemaDepth(child, fromGemini, depth+1)
				if err != nil {
					return nil, err
				}
				properties[name] = converted
			}
			out[key], err = json.Marshal(properties)
			if err != nil {
				return nil, err
			}
		case "items":
			out[key], err = convertGeminiSchemaDepth(value, fromGemini, depth+1)
			if err != nil {
				return nil, err
			}
		case "anyOf":
			var arms []json.RawMessage
			if json.Unmarshal(value, &arms) != nil || len(arms) == 0 {
				return nil, unsupported("function schema anyOf")
			}
			for i, arm := range arms {
				arms[i], err = convertGeminiSchemaDepth(arm, fromGemini, depth+1)
				if err != nil {
					return nil, err
				}
			}
			out[key], err = json.Marshal(arms)
			if err != nil {
				return nil, err
			}
		case "nullable":
			if !fromGemini {
				return nil, unsupported("non-JSON-Schema nullable keyword")
			}
			var nullable bool
			if string(value) == "null" || json.Unmarshal(value, &nullable) != nil {
				return nil, unsupported("schema nullable constraint")
			}
			// Applied after the rest of the schema has been converted.
		case "minItems", "maxItems", "minLength", "maxLength", "minProperties", "maxProperties":
			if fromGemini && len(value) > 0 && value[0] == '"' {
				var text string
				_ = json.Unmarshal(value, &text)
				number, err := strconv.ParseUint(text, 10, 63)
				if err != nil {
					return nil, unsupported("schema size constraint")
				}
				out[key] = json.RawMessage(strconv.FormatUint(number, 10))
			} else if !fromGemini {
				var n json.Number
				if json.Unmarshal(value, &n) != nil {
					return nil, unsupported("schema size constraint")
				}
				v, err := strconv.ParseUint(n.String(), 10, 63)
				if err != nil {
					return nil, unsupported("schema size constraint")
				}
				out[key], _ = json.Marshal(strconv.FormatUint(v, 10))
			} else {
				out[key] = value
			}
		case "title", "description", "format", "enum", "required", "minimum", "maximum", "pattern", "default", "example":
			out[key] = value
		default:
			return nil, unsupported("function schema keyword on the upstream protocol")
		}
	}
	var nullable bool
	_ = json.Unmarshal(members["nullable"], &nullable)
	if fromGemini && nullable {
		encoded, err := json.Marshal(out)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"anyOf": []json.RawMessage{encoded, json.RawMessage(`{"type":"null"}`)}})
	}
	return json.Marshal(out)
}
