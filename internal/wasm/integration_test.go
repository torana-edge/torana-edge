package wasm

import (
	"context"
	"os"
	"testing"
)

func TestDelegatorWASM(t *testing.T) {
	b, err := os.ReadFile("../../plugins/delegator/plugin.wasm")
	if err != nil {
		t.Skipf("delegator.wasm not found (skip if not built): %v", err)
		return
	}
	ctx := context.Background()
	r := NewRuntime(ctx)
	defer r.Close()

	p, err := r.LoadPlugin("delegator", b)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	_ = p
	t.Log("delegator.wasm loaded successfully")
}
