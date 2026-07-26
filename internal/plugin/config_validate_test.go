package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

func testSchema() *ConfigSchema {
	return &ConfigSchema{Fields: []ConfigField{
		{Key: "provider", Type: "string"},
		{Key: "max_scan_chars", Type: "number"},
		{Key: "enabled", Type: "boolean"},
		{Key: "on_error", Type: "enum", Options: []string{"block", "allow"}},
		{Key: "untyped"}, // absent type defaults to string
	}}
}

func validate(t *testing.T, cfg string) error {
	t.Helper()
	return ValidateConfigAgainstSchema(testSchema(), json.RawMessage(cfg))
}

// TestValidConfigAccepted — the ordinary case must pass untouched.
func TestValidConfigAccepted(t *testing.T) {
	if err := validate(t, `{"provider":"local","max_scan_chars":2048,"enabled":true,"on_error":"block","untyped":"x"}`); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

// TestWrongTypesRejected is the bug this validation exists for: before it, a
// string where a number belongs reached the guest and misbehaved silently.
func TestWrongTypesRejected(t *testing.T) {
	for _, tc := range []struct{ name, cfg, want string }{
		{"number given a string", `{"max_scan_chars":"2048"}`, "must be a number"},
		{"boolean given a string", `{"enabled":"true"}`, "must be a boolean"},
		{"string given a number", `{"provider":8080}`, "must be a string"},
		{"untyped field given a number", `{"untyped":1}`, "must be a string"},
		{"string given an object", `{"provider":{"a":1}}`, "must be a string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validate(t, tc.cfg); err == nil {
				t.Fatal("expected rejection, got none")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// TestEnumViolationRejected — an option the plugin does not implement is as
// broken as a wrong type, and the message must name the legal values.
func TestEnumViolationRejected(t *testing.T) {
	err := validate(t, `{"on_error":"explode"}`)
	if err == nil {
		t.Fatal("expected rejection of an undeclared enum value")
	}
	for _, want := range []string{"block", "allow", "explode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// TestUndeclaredKeysIgnored pins the deliberate scope limit. schema.json lists
// what the control plane renders, not everything a plugin accepts — Torana's own
// config.example.json proves it, so treating undeclared keys as suspect would
// fire on the project's shipped configuration.
func TestUndeclaredKeysIgnored(t *testing.T) {
	if err := validate(t, `{"provider":"local","povider":"typo","_comment":"docs","tool_policies":[{"match":"read*"}]}`); err != nil {
		t.Fatalf("undeclared keys must be ignored: %v", err)
	}
}

// TestShippedExampleConfigValidates is the concrete version of the rule above:
// the real compactor and intent stanzas from config.example.json must save
// cleanly against the real schemas those plugins ship.
func TestShippedExampleConfigValidates(t *testing.T) {
	compactorSchema := &ConfigSchema{Fields: []ConfigField{
		{Key: "max_offload_input_chars", Type: "number", Default: 0},
	}}
	compactorCfg := `{"_comment":"Dormant until compactor is added to order.",
		"max_offload_input_chars":0,"expected_applications":6,
		"tool_policies":[{"match":"read*","mode":"exact"}]}`
	if err := ValidateConfigAgainstSchema(compactorSchema, json.RawMessage(compactorCfg)); err != nil {
		t.Errorf("shipped compactor config rejected: %v", err)
	}

	intentSchema := &ConfigSchema{Fields: []ConfigField{
		{Key: "fill", Type: "enum", Options: []string{"heuristic", "off"}},
	}}
	intentCfg := `{"_comment":"fill controls…","fill":"heuristic"}`
	if err := ValidateConfigAgainstSchema(intentSchema, json.RawMessage(intentCfg)); err != nil {
		t.Errorf("shipped intent config rejected: %v", err)
	}
}

// TestNullMeansUnset — an explicit null falls back to the plugin's default and
// must not be type-checked against the declared type.
func TestNullMeansUnset(t *testing.T) {
	if err := validate(t, `{"max_scan_chars":null,"on_error":null}`); err != nil {
		t.Errorf("explicit nulls rejected: %v", err)
	}
}

// TestNoSchemaAcceptsAnything — most plugins ship no schema.json and must keep
// working exactly as before.
func TestNoSchemaAcceptsAnything(t *testing.T) {
	for _, schema := range []*ConfigSchema{nil, {}, {Fields: []ConfigField{}}} {
		if err := ValidateConfigAgainstSchema(schema, json.RawMessage(`{"anything":123}`)); err != nil {
			t.Errorf("schema %+v rejected a config: %v", schema, err)
		}
	}
}

// TestMalformedSchemaDoesNotBlockOperator — an enum with no options, or an
// unrecognised type, is the plugin author's bug. Rejecting the config would
// block an operator from fixing a problem they did not create.
func TestMalformedSchemaDoesNotBlockOperator(t *testing.T) {
	schema := &ConfigSchema{Fields: []ConfigField{
		{Key: "mode", Type: "enum"}, // no options declared
		{Key: "weird", Type: "quaternion"},
	}}
	if err := ValidateConfigAgainstSchema(schema, json.RawMessage(`{"mode":"anything","weird":42}`)); err != nil {
		t.Errorf("a malformed schema blocked a config write: %v", err)
	}
}

// TestNonObjectConfigPasses — the host stores the blob opaquely, so a config
// that is not an object cannot be checked field by field and is left alone.
func TestNonObjectConfigPasses(t *testing.T) {
	for _, cfg := range []string{`[1,2,3]`, `"a string"`, `42`, ``} {
		if err := ValidateConfigAgainstSchema(testSchema(), json.RawMessage(cfg)); err != nil {
			t.Errorf("config %q rejected: %v", cfg, err)
		}
	}
}

// TestErrorMessageIsStable — an error that differs between identical requests
// cannot be tested against or read comfortably.
func TestErrorMessageIsStable(t *testing.T) {
	cfg := `{"max_scan_chars":"x","enabled":"y","provider":1}`
	err := validate(t, cfg)
	if err == nil {
		t.Fatal("expected rejection")
	}
	want := err.Error()
	for i := 0; i < 20; i++ {
		if got := validate(t, cfg); got == nil || got.Error() != want {
			t.Fatalf("message not stable across runs:\n  %v\n  %v", want, got)
		}
	}
}
