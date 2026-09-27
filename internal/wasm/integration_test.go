package wasm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/torana-edge/torana-edge/internal/testfixture"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// requireWASM skips locally when the plugin binary is missing but fails in
// CI (TORANA_E2E=1) so missing binaries can never silently disable coverage.
func requireWASM(t *testing.T, path string) { testfixture.Require(t, path) }

// TestLoadRealPlugins loads every in-repo plugin binary and validates that
// each exports the hooks its manifest declares.
func TestLoadRealPlugins(t *testing.T) {
	dir := officialBundlesDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	r := NewRuntime(ctx)
	defer r.Close()

	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		count++
		name := entry.Name()
		t.Run(name, func(t *testing.T) {
			// The supplied bundle is authoritative. A second hard-coded hook
			// table drifts whenever a plugin adds a hook, and also misses new
			// plugins entirely. The catalogue owns its exact release inventory.
			manifestBytes, err := os.ReadFile(filepath.Join(dir, name, "plugin.json"))
			if err != nil {
				t.Fatal(err)
			}
			var manifest struct {
				Name  string `json:"name"`
				Hooks []struct {
					Name string `json:"name"`
				} `json:"hooks"`
			}
			if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.Name != name || len(manifest.Hooks) == 0 {
				t.Fatal("invalid bundle identity or empty hook declaration")
			}
			path := filepath.Join(dir, name, "plugin.wasm")
			requireWASM(t, path)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			p, err := r.LoadPlugin(name, b)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			declared := make([]pb.Hook, 0, len(manifest.Hooks))
			seen := map[string]bool{}
			for _, h := range manifest.Hooks {
				hk, ok := sdk.ManifestHookName(h.Name)
				if !ok {
					t.Fatalf("unknown hook %q in the bundle", h.Name)
				}
				if seen[h.Name] {
					t.Fatalf("duplicate hook %q", h.Name)
				}
				seen[h.Name] = true
				declared = append(declared, hk)
			}
			if err := p.ValidateHooks(ctx, declared); err != nil {
				t.Fatalf("hooks: %v", err)
			}
		})
	}
	if count == 0 {
		t.Fatal("official bundle directory contains no plugins")
	}
}
