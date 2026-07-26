package plugin

import (
	"encoding/json"
	"testing"
)

func TestValidateAgentPayload(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"required":["status","capabilities"],
		"properties":{
			"status":{"const":"ready"},
			"capabilities":{"type":"array","items":{"type":"string"}}
		},
		"additionalProperties":false
	}`)
	if err := ValidateAgentSchema(schema); err != nil {
		t.Fatalf("valid schema: %v", err)
	}
	if err := ValidateAgentPayload(schema, json.RawMessage(`{"status":"ready","capabilities":["metrics"]}`)); err != nil {
		t.Fatalf("valid payload: %v", err)
	}
	for _, payload := range []string{
		`{"status":"starting","capabilities":[]}`,
		`{"status":"ready"}`,
		`{"status":"ready","capabilities":[1]}`,
		`{"status":"ready","capabilities":[],"extra":true}`,
	} {
		if err := ValidateAgentPayload(schema, json.RawMessage(payload)); err == nil {
			t.Fatalf("invalid payload accepted: %s", payload)
		}
	}
	if err := ValidateAgentPayload(nil, nil); err != nil {
		t.Fatalf("bodyless operation: %v", err)
	}
	if err := ValidateAgentPayload(nil, json.RawMessage(`{}`)); err == nil {
		t.Fatal("body accepted without input schema")
	}
}

func TestValidateAgentSchemaRejectsUnsupportedKeywords(t *testing.T) {
	if err := ValidateAgentSchema(json.RawMessage(`{"type":"string","pattern":"secret"}`)); err == nil {
		t.Fatal("unsupported schema keyword was accepted")
	}
}

func TestValidateAgentPayloadUsesExactIntegers(t *testing.T) {
	schema := json.RawMessage(`{"type":"integer"}`)
	if err := ValidateAgentPayload(schema, json.RawMessage(`9007199254740993`)); err != nil {
		t.Fatalf("large integer rejected: %v", err)
	}
	if err := ValidateAgentPayload(schema, json.RawMessage(`9007199254740993.1`)); err == nil {
		t.Fatal("large fractional value rounded into an integer")
	}
}
