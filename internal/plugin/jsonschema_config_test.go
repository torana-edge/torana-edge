package plugin

import (
	"encoding/json"
	"os"
	"testing"
)

// The two repositories ship different schema.json formats — Torana's UI manifest
// here, JSON Schema in the official plugins repo — and the mismatch was silent:
// a JSON Schema document unmarshals into ConfigSchema cleanly and yields zero
// fields, so every official plugin rendered no config form and nothing errored.

// TestOfficialPluginSchemaRendersFields is the regression test for that. It
// reads the real shape the official repository publishes.
func TestOfficialPluginSchemaRendersFields(t *testing.T) {
	raw := []byte(`{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"provider": {"type": "string", "description": "Configured local provider."},
			"on_error": {"type": "string", "enum": ["block", "allow"], "default": "block",
			             "description": "Fail closed or open."},
			"max_scan_chars": {"type": "integer", "minimum": 0, "default": 0},
			"tools": {"type": "array", "items": {"type": "string"}}
		}
	}`)

	// The old path: parses fine, produces nothing. This is what shipped.
	var direct ConfigSchema
	if err := json.Unmarshal(raw, &direct); err != nil {
		t.Fatalf("JSON Schema failed to unmarshal at all: %v", err)
	}
	if len(direct.Fields) != 0 {
		t.Fatal("premise changed: ConfigSchema now parses JSON Schema directly")
	}

	got := deriveConfigSchema(raw)
	if got == nil {
		t.Fatal("derived nothing from a valid JSON Schema config document")
	}

	byKey := map[string]ConfigField{}
	for _, f := range got.Fields {
		byKey[f.Key] = f
	}

	if f := byKey["provider"]; f.Type != "string" || f.Help != "Configured local provider." {
		t.Errorf("provider = %+v", f)
	}
	if f := byKey["on_error"]; f.Type != "enum" || len(f.Options) != 2 || f.Default != "block" {
		t.Errorf("on_error = %+v, want an enum with two options and a default", f)
	}
	if f := byKey["max_scan_chars"]; f.Type != "number" {
		t.Errorf("integer did not map to a number control: %+v", f)
	}
	// An array cannot be rendered by any control the form has, and guessing
	// would corrupt the value on save.
	if _, rendered := byKey["tools"]; rendered {
		t.Error("an array property was rendered as a scalar control")
	}
}

// TestDerivedFieldOrderIsStable — JSON Schema properties are an object and have
// no order, so a form that reordered between reloads would be worse than one
// that is merely alphabetical.
func TestDerivedFieldOrderIsStable(t *testing.T) {
	raw := []byte(`{"type":"object","properties":{
		"zeta":{"type":"string"},"alpha":{"type":"string"},"mu":{"type":"string"}}}`)

	first := deriveConfigSchema(raw)
	for i := 0; i < 20; i++ {
		got := deriveConfigSchema(raw)
		for j := range got.Fields {
			if got.Fields[j].Key != first.Fields[j].Key {
				t.Fatalf("field order is not stable: %v then %v", first.Fields, got.Fields)
			}
		}
	}
	if first.Fields[0].Key != "alpha" {
		t.Errorf("expected alphabetical order, got %s first", first.Fields[0].Key)
	}
}

// TestToranaFieldsFormatStillWins — the native format must keep working
// unchanged, including its explicit ordering.
func TestToranaFieldsFormatStillWins(t *testing.T) {
	raw := []byte(`{"fields":[{"key":"zeta","type":"string"},{"key":"alpha","type":"number"}]}`)
	var s ConfigSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Fields) != 2 || s.Fields[0].Key != "zeta" {
		t.Errorf("native format changed: %+v", s.Fields)
	}
	// And it must not be second-guessed by the derivation.
	if deriveConfigSchema(raw) != nil {
		t.Error("derivation fired on a document that already had fields")
	}
}

// TestDeriveRejectsNonConfigDocuments — anything that is not a config object
// must fall through rather than produce a nonsense form.
func TestDeriveRejectsNonConfigDocuments(t *testing.T) {
	for _, raw := range []string{
		`not json`,
		`{"type":"array","items":{"type":"string"}}`,
		`{"type":"object"}`,
		`{"type":"object","properties":{}}`,
		`{"type":"object","properties":{"only":{"type":"array"}}}`,
	} {
		if got := deriveConfigSchema([]byte(raw)); got != nil {
			t.Errorf("derived %+v from %q", got.Fields, raw)
		}
	}
}

// TestSourceExtensionSurvives — the picker extension has to travel through JSON
// Schema too, or a plugin published to the official repo loses it.
func TestSourceExtensionSurvives(t *testing.T) {
	raw := []byte(`{"type":"object","properties":{
		"conversations":{"type":"string","x-torana-source":"conversations"}}}`)
	got := deriveConfigSchema(raw)
	if got == nil || len(got.Fields) != 1 {
		t.Fatalf("derived %+v", got)
	}
	if got.Fields[0].Source != "conversations" {
		t.Errorf("Source = %q, want the picker to survive", got.Fields[0].Source)
	}
}

// countScalarProperties counts JSON Schema properties a form could render.
func countScalarProperties(raw []byte) int {
	var doc struct {
		Properties map[string]struct {
			Type string `json:"type"`
			Enum []any  `json:"enum"`
		} `json:"properties"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return 0
	}
	n := 0
	for _, p := range doc.Properties {
		switch {
		case len(p.Enum) > 0, p.Type == "string", p.Type == "number",
			p.Type == "integer", p.Type == "boolean":
			n++
		}
	}
	return n
}

// TestRealOfficialPluginSchemas walks the schemas actually on disk. If the
// official repo is checked out alongside, every one of its plugins should now
// render a form.
func TestRealOfficialPluginSchemas(t *testing.T) {
	const dir = "../../../torana-plugins/plugins"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skip("official plugins repo not checked out alongside")
	}
	checked := 0
	for _, e := range entries {
		raw, err := os.ReadFile(dir + "/" + e.Name() + "/schema.json")
		if err != nil {
			continue
		}
		var s ConfigSchema
		if json.Unmarshal(raw, &s) != nil {
			t.Errorf("%s: schema.json does not parse", e.Name())
			continue
		}
		// Rendering no form is correct for a plugin with no settings, and for
		// one whose settings are all arrays or nested objects — no scalar
		// control can carry those, and guessing would corrupt them on save.
		// The bug this guards is a schema full of perfectly renderable strings
		// and numbers that produces nothing.
		if len(s.Fields) == 0 && deriveConfigSchema(raw) == nil && countScalarProperties(raw) > 0 {
			t.Errorf("%s: schema.json declares %d scalar settings but renders no form",
				e.Name(), countScalarProperties(raw))
		}
		checked++
	}
	if checked == 0 {
		t.Skip("no schemas found")
	}
	t.Logf("checked %d official plugin schemas", checked)
}
