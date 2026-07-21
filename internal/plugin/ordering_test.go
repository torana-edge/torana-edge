package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/wasm"
)

func TestOrderingConstraintViolation(t *testing.T) {
	dir := t.TempDir()
	pGateDir := filepath.Join(dir, "gate_plugin")
	pRouterDir := filepath.Join(dir, "router_plugin")

	if err := os.MkdirAll(pGateDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pRouterDir, 0755); err != nil {
		t.Fatal(err)
	}

	gateManifest := PluginManifest{
		Name:        "gate_plugin",
		Version:     "0.1.0",
		Description: "compaction gate plugin",
		Permissions: []Permission{
			{Name: "env.host_call.torana_evaluate_compaction"},
		},
	}
	routerManifest := PluginManifest{
		Name:        "router_plugin",
		Version:     "0.1.0",
		Description: "route capable plugin",
		Permissions: []Permission{
			{Name: "env.route_request"},
		},
	}

	gateJSON, _ := json.Marshal(gateManifest)
	routerJSON, _ := json.Marshal(routerManifest)

	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

	os.WriteFile(filepath.Join(pGateDir, "plugin.json"), gateJSON, 0644)
	os.WriteFile(filepath.Join(pGateDir, "plugin.wasm"), wasmBytes, 0644)
	os.WriteFile(filepath.Join(pRouterDir, "plugin.json"), routerJSON, 0644)
	os.WriteFile(filepath.Join(pRouterDir, "plugin.wasm"), wasmBytes, 0644)

	rt := wasm.NewRuntime(context.Background())
	defer rt.Close()

	// Invalid order: gate_plugin before router_plugin -> must fail with ordering constraint violation
	invalidCfg := PluginConfig{
		Dir:   dir,
		Order: []string{"gate_plugin", "router_plugin"},
	}

	_, err := reloadPipeline(rt, invalidCfg)
	if err == nil {
		t.Fatal("expected ordering constraint error, got nil")
	}
	if !strings.Contains(err.Error(), "ordering constraint violation") {
		t.Errorf("unexpected error message: %v", err)
	}

	// Valid order: router_plugin before gate_plugin -> should pass ordering validation
	validCfg := PluginConfig{
		Dir:   dir,
		Order: []string{"router_plugin", "gate_plugin"},
	}

	// reloadPipeline will try to compile the dummy wasm modules.
	// Ordering check happens before loading, so ordering error won't be returned.
	_, err2 := reloadPipeline(rt, validCfg)
	if err2 != nil && strings.Contains(err2.Error(), "ordering constraint violation") {
		t.Errorf("valid order returned ordering constraint violation: %v", err2)
	}
}
