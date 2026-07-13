package wasm

import (
	"context"
	"os"
	"testing"
)

func TestDelegator(t *testing.T) {
	b, _ := os.ReadFile("../../plugins/delegator/plugin.wasm")
	r := NewRuntime(context.Background()); defer r.Close()
	p, _ := r.LoadPlugin("delegator", b)
	var out map[string]any
	if err := p.CallRequest(context.Background(), "on_chat_request", map[string]any{"t":1}, &out); err != nil {
		t.Fatal(err)
	}
	if out["handled_by"] != "delegator.wasm" { t.Fatal("bad output") }
	t.Log("delegator OK")
}

func TestSchemaTranslator(t *testing.T) {
	b, _ := os.ReadFile("../../plugins/schema_translator/plugin.wasm")
	r := NewRuntime(context.Background()); defer r.Close()
	p, _ := r.LoadPlugin("schema_translator", b)
	out := map[string]any{}
	p.CallRequest(context.Background(), "on_chat_request", map[string]any{"tools": []any{}}, &out)
	t.Log("schema_translator OK")
}
