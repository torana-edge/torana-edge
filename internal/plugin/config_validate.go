package plugin

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ValidateConfigAgainstSchema checks a plugin's config blob against the
// ConfigSchema its bundle declares in schema.json.
//
// Until now the only check on this path was json.Valid, so a string where a
// number belongs, or an enum value the plugin does not implement, reached the
// guest and misbehaved silently. This makes the declared parts of the schema a
// contract.
//
// Only declared fields are checked, and undeclared keys are ignored entirely.
// That is not leniency for its own sake: schema.json is a UI manifest listing
// the fields the control plane renders, never an exhaustive list of accepted
// settings. Torana's own config.example.json relies on this — the compactor
// reads expected_applications and tool_policies while declaring neither, and
// every stanza carries a _comment key. Treating undeclared keys as suspect would
// fire on the project's own shipped configuration, and rejecting them would make
// the schema form unsaveable for anyone whose config predates their plugin's
// schema.
//
// A bundle with no schema, or one declaring no fields, accepts anything — most
// plugins ship no schema.json and must keep working.
func ValidateConfigAgainstSchema(schema *ConfigSchema, raw json.RawMessage) error {
	if schema == nil || len(schema.Fields) == 0 || len(raw) == 0 {
		return nil
	}

	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		// A non-object config cannot be checked field by field. That is not
		// necessarily wrong — the host stores the blob opaquely — so let it pass
		// and leave the shape to the plugin.
		return nil
	}

	var problems []string
	for _, field := range schema.Fields {
		if field.Key == "" {
			continue
		}
		value, present := cfg[field.Key]
		if !present || value == nil {
			// Absent, or an explicit null: the plugin falls back to its default.
			continue
		}
		if problem := checkFieldValue(field, value); problem != "" {
			problems = append(problems, problem)
		}
	}
	if len(problems) == 0 {
		return nil
	}

	// Schema order is the author's order, but sort anyway so the message is
	// identical for identical input regardless of how the fields were listed.
	sort.Strings(problems)
	return fmt.Errorf("%s", strings.Join(problems, "; "))
}

// checkFieldValue returns a human-readable problem, or "" when the value is
// acceptable for the declared field.
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
			// A malformed schema, not a bad config. Reporting it here would
			// block an operator from fixing a problem they did not create.
			return ""
		}
		for _, opt := range field.Options {
			if s == opt {
				return ""
			}
		}
		return fmt.Sprintf("%q must be one of [%s], got %q", field.Key, strings.Join(field.Options, ", "), s)
	case "string", "":
		// An absent type defaults to string, matching how the control plane
		// renders an unrecognised field.
		if _, ok := value.(string); !ok {
			return fmt.Sprintf("%q must be a string, got %s", field.Key, jsonTypeName(value))
		}
	default:
		// An unrecognised declared type is a schema bug. Checking against it
		// would reject configs the operator cannot fix.
		return ""
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
