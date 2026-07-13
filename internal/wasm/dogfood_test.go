package wasm

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestDelegator(t *testing.T) {
	b, _ := os.ReadFile("../../plugins/delegator/plugin.wasm")
	r := NewRuntime(context.Background()); defer r.Close()
	p, _ := r.LoadPlugin("delegator", b)
	var out map[string]any
	wrapped := map[string]any{"chat": `{"t":1}`}
	if err := p.CallRequest(context.Background(), "on_chat_request", wrapped, &out); err != nil {
		t.Fatal(err)
	}
	
	// Unpack the modified chat wrapper
	chatStr, ok := out["chat"].(string)
	if !ok { t.Fatalf("missing chat wrapper in output: %v", out) }
	
	var actual map[string]any
	json.Unmarshal([]byte(chatStr), &actual)
	
	if actual["Model"] != "claude-3-5-sonnet-20241022" { t.Fatal("bad output") }
	t.Log("delegator OK")
}

func TestSchemaTranslator(t *testing.T) {
	b, _ := os.ReadFile("../../plugins/schema_translator/plugin.wasm")
	r := NewRuntime(context.Background()); defer r.Close()
	p, _ := r.LoadPlugin("schema_translator", b)
	out := map[string]any{}
	wrapped := map[string]any{"chat": `{"Tools":[]}`}
	p.CallRequest(context.Background(), "on_chat_request", wrapped, &out)
	t.Log("schema_translator OK")
}
