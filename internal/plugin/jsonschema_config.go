package plugin

import (
	"encoding/json"
	"sort"
)

// Two shapes of schema.json exist in the wild, and the host must read both.
//
// Torana's own shape is a UI manifest: {"fields":[{key,type,label,...}]}. The
// official plugins repository instead ships JSON Schema (draft 2020-12) and
// validates it in CI, because JSON Schema is the standard way to describe a
// config object and it carries constraints this host does not model.
//
// The two drifted apart without anyone noticing, and the symptom was silent:
// unmarshalling a JSON Schema document into ConfigSchema succeeds and yields
// zero fields, so every official plugin rendered no configuration form at all
// and fell back to the raw JSON editor. Nothing errored, so nothing was
// reported.
//
// Rather than force one repository to abandon its format, the host derives UI
// fields from JSON Schema when a document has no "fields" key. Authors can
// write either.
//
// Only what the control plane can render is derived — string, number, boolean,
// and enum. Everything else JSON Schema can express (arrays, nested objects,
// numeric bounds) is left to the raw editor rather than misrepresented by a
// control that cannot honour it.

// jsonSchemaProperty is the subset of a JSON Schema property this maps from.
type jsonSchemaProperty struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Default     any    `json:"default"`
	Enum        []any  `json:"enum"`
	// Source is a Torana extension naming a live host resource the control
	// plane can offer as a picker. JSON Schema permits unknown keywords, so
	// this travels without making the document invalid.
	Source string `json:"x-torana-source"`
}

// deriveConfigSchema converts a JSON Schema config document into the UI fields
// the control plane renders. It returns nil when raw is not a JSON Schema
// object, so a caller can fall back.
func deriveConfigSchema(raw []byte) *ConfigSchema {
	var doc struct {
		Type       string                        `json:"type"`
		Properties map[string]jsonSchemaProperty `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	if doc.Type != "object" || len(doc.Properties) == 0 {
		return nil
	}

	// JSON Schema properties are an object, and object keys have no order, so
	// the form order is alphabetical. Deterministic beats arbitrary: a form
	// whose fields move between reloads is worse than one that is merely not
	// in the author's preferred order.
	keys := make([]string, 0, len(doc.Properties))
	for k := range doc.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := &ConfigSchema{}
	for _, key := range keys {
		prop := doc.Properties[key]
		field := ConfigField{
			Key:     key,
			Label:   prop.Title,
			Help:    prop.Description,
			Default: prop.Default,
			Source:  prop.Source,
		}
		if field.Label == "" {
			field.Label = key
		}
		switch {
		case len(prop.Enum) > 0:
			field.Type = "enum"
			for _, v := range prop.Enum {
				if s, ok := v.(string); ok {
					field.Options = append(field.Options, s)
				}
			}
			// A non-string enum cannot be rendered as a select whose values
			// round-trip, so leave it to the raw editor.
			if len(field.Options) != len(prop.Enum) {
				continue
			}
		case prop.Type == "string":
			field.Type = "string"
		case prop.Type == "number" || prop.Type == "integer":
			field.Type = "number"
		case prop.Type == "boolean":
			field.Type = "boolean"
		default:
			// array, object, null, or absent: not renderable as a scalar
			// control, and guessing would produce a form that silently
			// corrupts the value on save.
			continue
		}
		out.Fields = append(out.Fields, field)
	}
	if len(out.Fields) == 0 {
		return nil
	}
	return out
}
