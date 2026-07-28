package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ValidateConfigAgainstSchema validates the entire plugin configuration.
// JSON Schema documents use Draft 2020-12 validation; the legacy {"fields":[]}
// UI format retains its scalar checks for compatibility.
func ValidateConfigAgainstSchema(schema *ConfigSchema, raw json.RawMessage) error {
	if schema == nil || len(raw) == 0 {
		return nil
	}
	if len(schema.Raw) == 0 {
		return validateLegacyFields(schema, raw)
	}

	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("config must be valid JSON: %w", err)
	}
	// Older Torana examples embedded prose in plugin config as `_comment`.
	// It is host documentation metadata, not a plugin setting. Preserve that
	// one compatibility exception while validating every other property.
	if object, ok := instance.(map[string]any); ok {
		delete(object, "_comment")
	}

	compiled, ok := schema.compiledSchema.(*jsonschema.Schema)
	if !ok {
		if err := prepareConfigSchema(schema); err != nil {
			return err
		}
		compiled = schema.compiledSchema.(*jsonschema.Schema)
	}
	if err := compiled.Validate(instance); err != nil {
		return fmt.Errorf("config does not match schema: %w", err)
	}
	return nil
}

func prepareConfigSchema(schema *ConfigSchema) error {
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema.Raw))
	if err != nil {
		return fmt.Errorf("invalid plugin schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	const resource = "torana-plugin-config.json"
	if err := compiler.AddResource(resource, schemaDoc); err != nil {
		return fmt.Errorf("invalid plugin schema: %w", err)
	}
	compiled, err := compiler.Compile(resource)
	if err != nil {
		return fmt.Errorf("invalid plugin schema: %w", err)
	}
	schema.compiledSchema = compiled
	return nil
}

func validateLegacyFields(schema *ConfigSchema, raw json.RawMessage) error {
	if len(schema.Fields) == 0 {
		return nil
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil
	}
	var problems []string
	for _, field := range schema.Fields {
		if field.Key == "" {
			continue
		}
		value, present := cfg[field.Key]
		if !present || value == nil {
			continue
		}
		if problem := checkFieldValue(field, value); problem != "" {
			problems = append(problems, problem)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(problems, "; "))
}

func checkFieldValue(field ConfigField, value any) string {
	switch field.Type {
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Sprintf("%q must be a boolean, got %s", field.Key, jsonTypeName(value))
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return fmt.Sprintf("%q must be a number, got %s", field.Key, jsonTypeName(value))
		}
	case "enum":
		s, ok := value.(string)
		if !ok {
			return fmt.Sprintf("%q must be one of [%s], got %s", field.Key, strings.Join(field.Options, ", "), jsonTypeName(value))
		}
		if len(field.Options) == 0 {
			return ""
		}
		for _, opt := range field.Options {
			if s == opt {
				return ""
			}
		}
		return fmt.Sprintf("%q must be one of [%s], got %q", field.Key, strings.Join(field.Options, ", "), s)
	case "string", "":
		if _, ok := value.(string); !ok {
			return fmt.Sprintf("%q must be a string, got %s", field.Key, jsonTypeName(value))
		}
	}
	return ""
}

func jsonTypeName(v any) string {
	switch v.(type) {
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	case nil:
		return "null"
	}
	return "an unknown type"
}
