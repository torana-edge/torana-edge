package wasm

import (
	"context"
	"os"
	"testing"
)

// requireWASM skips locally when the plugin binary is missing but fails in
// CI (TORANA_E2E=1) so missing binaries can never silently disable coverage.
func requireWASM(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		if os.Getenv("TORANA_E2E") != "" {
			t.Fatalf("%s missing — run 'make plugins testdata' (err: %v)", path, err)
		}
		t.Skipf("%s not built — run 'make plugins testdata'", path)
	}
}

// TestLoadRealPlugins loads every in-repo plugin binary and validates that
// each exports the hooks its manifest declares.
func TestLoadRealPlugins(t *testing.T) {
	cases := map[string][]string{
		"schema_translator": {"run_before_request", "run_on_stream_chunk"},
		"keyword_compactor": {"run_before_request"},
		"compactor":         {"run_before_request", "run_on_stream_chunk"},
		"otel":              {"run_before_request", "run_after_response"},
		"auth":              {"run_before_request"},
	}

	ctx := context.Background()
	r := NewRuntime(ctx)
	defer r.Close()

	for name, hooks := range cases {
		t.Run(name, func(t *testing.T) {
			path := "../../plugins/" + name + "/plugin.wasm"
			requireWASM(t, path)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			p, err := r.LoadPlugin(name, b)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if err := p.ValidateHooks(ctx, hooks); err != nil {
				t.Fatalf("hooks: %v", err)
			}
		})
	}
}
