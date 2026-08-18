package wasm

import (
	"context"
	"os"
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// requireWASM skips locally when the plugin binary is missing but fails in
// CI (TORANA_E2E=1) so missing binaries can never silently disable coverage.
func requireWASM(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		if os.Getenv("TORANA_E2E") != "" {
			t.Fatalf("%s missing — run 'make testdata' (err: %v)", path, err)
		}
		t.Skipf("%s not built — run 'make testdata'", path)
	}
}

// TestLoadRealPlugins loads every in-repo plugin binary and validates that
// each exports the hooks its manifest declares.
func TestLoadRealPlugins(t *testing.T) {
	cases := map[string][]string{
		"schema_translator":   {"run_before_request", "run_on_stream_chunk"},
		"keyword_compactor":   {"run_before_request"},
		"compactor":           {"run_before_request"},
		"otel":                {"run_before_request", "run_after_response", "run_on_http_request"},
		"auth":                {"run_before_request"},
		"intent":              {"run_before_request", "run_on_stream_chunk"},
		"pii":                 {"run_before_request"},
		"cache_warmer":        {"run_before_request", "run_on_tick"},
		"cache_tier_selector": {"run_before_request"},
		"tool_governor":       {"run_before_request"},
	}

	ctx := context.Background()
	r := NewRuntime(ctx)
	defer r.Close()

	for name, hooks := range cases {
		t.Run(name, func(t *testing.T) {
			path := officialBundlesDir(t) + "/" + name + "/plugin.wasm"
			requireWASM(t, path)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			p, err := r.LoadPlugin(name, b)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			declared := make([]pb.Hook, 0, len(hooks))
			for _, h := range hooks {
				hk, ok := sdk.ManifestHookName(h)
				if !ok {
					t.Fatalf("unknown hook %q in the test table", h)
				}
				declared = append(declared, hk)
			}
			if err := p.ValidateHooks(ctx, declared); err != nil {
				t.Fatalf("hooks: %v", err)
			}
		})
	}
}
