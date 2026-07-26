package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
)

var supportedAgentSchemaKeywords = map[string]struct{}{
	"$schema":              {},
	"title":                {},
	"description":          {},
	"type":                 {},
	"properties":           {},
	"required":             {},
	"additionalProperties": {},
	"items":                {},
	"const":                {},
	"enum":                 {},
}

// ValidateAgentSchema validates the deliberately small JSON Schema subset
// Torana v1 can enforce at the plugin boundary.
func ValidateAgentSchema(raw json.RawMessage) error {
	var schema map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &schema) != nil || schema == nil {
		return fmt.Errorf("must be a JSON object")
	}
	for keyword := range schema {
		if _, ok := supportedAgentSchemaKeywords[keyword]; !ok {
			return fmt.Errorf("unsupported schema keyword %q", keyword)
		}
	}

	var schemaType string
	if rawType, ok := schema["type"]; ok {
		if err := json.Unmarshal(rawType, &schemaType); err != nil {
			return fmt.Errorf("type must be a string")
		}
		switch schemaType {
		case "object", "array", "string", "number", "integer", "boolean", "null":
		default:
			return fmt.Errorf("unsupported type %q", schemaType)
		}
	}
	if schemaType == "" && schema["const"] == nil && schema["enum"] == nil {
		return fmt.Errorf("requires type, const, or enum")
	}

	var properties map[string]json.RawMessage
	if rawProperties, ok := schema["properties"]; ok {
		if schemaType != "object" {
			return fmt.Errorf("properties requires object type")
		}
		if err := json.Unmarshal(rawProperties, &properties); err != nil || properties == nil {
			return fmt.Errorf("properties must be an object")
		}
		for name, child := range properties {
			if name == "" {
				return fmt.Errorf("property name cannot be empty")
			}
			if err := ValidateAgentSchema(child); err != nil {
				return fmt.Errorf("property %q: %w", name, err)
			}
		}
	}

	if rawRequired, ok := schema["required"]; ok {
		if schemaType != "object" {
			return fmt.Errorf("required requires object type")
		}
		var required []string
		if err := json.Unmarshal(rawRequired, &required); err != nil {
			return fmt.Errorf("required must be an array of strings")
		}
		seen := make(map[string]struct{}, len(required))
		for _, name := range required {
			if name == "" {
				return fmt.Errorf("required property cannot be empty")
			}
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("required property %q is duplicated", name)
			}
			seen[name] = struct{}{}
			if properties != nil {
				if _, exists := properties[name]; !exists {
					return fmt.Errorf("required property %q is not declared", name)
				}
			}
		}
	}

	if rawAdditional, ok := schema["additionalProperties"]; ok {
		if schemaType != "object" {
			return fmt.Errorf("additionalProperties requires object type")
		}
		var allowed bool
		if err := json.Unmarshal(rawAdditional, &allowed); err != nil {
			return fmt.Errorf("additionalProperties must be boolean")
		}
	}

	if rawItems, ok := schema["items"]; ok {
		if schemaType != "array" {
			return fmt.Errorf("items requires array type")
		}
		if err := ValidateAgentSchema(rawItems); err != nil {
			return fmt.Errorf("items: %w", err)
		}
	} else if schemaType == "array" {
		return fmt.Errorf("array type requires items")
	}

	if rawEnum, ok := schema["enum"]; ok {
		var values []json.RawMessage
		if err := json.Unmarshal(rawEnum, &values); err != nil || len(values) == 0 {
			return fmt.Errorf("enum must be a non-empty array")
		}
	}
	for _, keyword := range []string{"$schema", "title", "description"} {
		if value, ok := schema[keyword]; ok {
			var text string
			if err := json.Unmarshal(value, &text); err != nil {
				return fmt.Errorf("%s must be a string", keyword)
			}
		}
	}
	return nil
}

// ValidateAgentPayload validates one JSON value against Torana's enforceable
// agent-schema subset. A missing schema means that the operation accepts no
// body.
func ValidateAgentPayload(schema, payload json.RawMessage) error {
	if len(schema) == 0 {
		if len(payload) == 0 {
			return nil
		}
		return fmt.Errorf("operation does not accept a request body")
	}
	if len(payload) == 0 {
		return fmt.Errorf("JSON body is required")
	}
	if err := ValidateAgentSchema(schema); err != nil {
		return fmt.Errorf("invalid agent schema: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return validateAgentSchemaValue(schema, value, "$")
}

func validateAgentSchemaValue(raw json.RawMessage, value any, path string) error {
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(raw, &schema); err != nil {
		return fmt.Errorf("%s: invalid schema: %w", path, err)
	}
	if expected, ok := schema["const"]; ok {
		var constant any
		decoder := json.NewDecoder(bytes.NewReader(expected))
		decoder.UseNumber()
		if err := decoder.Decode(&constant); err != nil || !reflect.DeepEqual(value, constant) {
			return fmt.Errorf("%s must equal the declared constant", path)
		}
	}
	if rawEnum, ok := schema["enum"]; ok {
		var encoded []json.RawMessage
		_ = json.Unmarshal(rawEnum, &encoded)
		matched := false
		for _, candidateRaw := range encoded {
			var candidate any
			decoder := json.NewDecoder(bytes.NewReader(candidateRaw))
			decoder.UseNumber()
			if decoder.Decode(&candidate) == nil && reflect.DeepEqual(value, candidate) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s is not an allowed value", path)
		}
	}

	var schemaType string
	_ = json.Unmarshal(schema["type"], &schemaType)
	switch schemaType {
	case "":
		return nil
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object", path)
		}
		var properties map[string]json.RawMessage
		_ = json.Unmarshal(schema["properties"], &properties)
		var required []string
		_ = json.Unmarshal(schema["required"], &required)
		for _, name := range required {
			if _, exists := object[name]; !exists {
				return fmt.Errorf("%s.%s is required", path, name)
			}
		}
		additionalAllowed := true
		if rawAdditional, ok := schema["additionalProperties"]; ok {
			_ = json.Unmarshal(rawAdditional, &additionalAllowed)
		}
		for name, childValue := range object {
			childSchema, declared := properties[name]
			if !declared {
				if !additionalAllowed {
					return fmt.Errorf("%s.%s is not allowed", path, name)
				}
				continue
			}
			if err := validateAgentSchemaValue(childSchema, childValue, path+"."+name); err != nil {
				return err
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be an array", path)
		}
		for index, item := range array {
			if err := validateAgentSchemaValue(schema["items"], item, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be a string", path)
		}
	case "number":
		if _, ok := value.(json.Number); !ok {
			return fmt.Errorf("%s must be a number", path)
		}
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return fmt.Errorf("%s must be an integer", path)
		}
		rational, ok := new(big.Rat).SetString(number.String())
		if !ok || !rational.IsInt() {
			return fmt.Errorf("%s must be an integer", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be a boolean", path)
		}
	case "null":
		if value != nil {
			return fmt.Errorf("%s must be null", path)
		}
	}
	return nil
}
