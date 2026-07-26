package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/torana-edge/torana-edge/internal/wasm"
	"github.com/torana-edge/torana-edge/sdk/pb"
)

// TestRunOnHTTPRequest_UnknownPlugin verifies that RunOnHTTPRequest returns
// (nil, nil) when the named plugin is not present in the pipeline, so the
// proxy route handler can map it to a 404.
func TestRunOnHTTPRequest_UnknownPlugin(t *testing.T) {
	rt := wasm.NewRuntime(context.Background())
	pp, err := NewPipeline(rt, PluginConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	defer rt.Close()

	resp, err := pp.RunOnHTTPRequest(context.Background(), 1, "nonexistent", &pb.HttpRequest{
		Method: "GET",
		Path:   "/",
	})
	if err != nil {
		t.Fatalf("expected (nil, nil) for unknown plugin, got error: %v", err)
	}
	if resp != nil {
		t.Fatalf("expected nil response for unknown plugin, got: %+v", resp)
	}
}

// TestRunOnHTTPRequest_ForbiddenWithoutGrant loads the otel plugin but
// temporarily strips the env.serve_http grant to verify RunOnHTTPRequest
// returns ErrServeHTTPForbidden, which the proxy maps to 403.
//
// This test uses the real otel WASM binary. When the binary is absent it is
// skipped locally (and fails loudly in CI with TORANA_E2E=1).
func TestRunOnHTTPRequest_ForbiddenWithoutGrant(t *testing.T) {
	requireWASM(t, "../../plugins/otel/plugin.wasm")

	rt := wasm.NewRuntime(context.Background())
	defer rt.Close()

	// Load the pipeline with otel but intentionally declare NO permissions
	// so env.serve_http is absent.
	bundles, err := DiscoverPlugins("../../plugins")
	if err != nil {
		t.Fatalf("DiscoverPlugins: %v", err)
	}
	var otelBundle *PluginBundle
	for i := range bundles {
		if bundles[i].Manifest.Name == "otel" {
			otelBundle = &bundles[i]
			break
		}
	}
	if otelBundle == nil {
		t.Skip("otel bundle not found in plugins dir")
	}

	// Strip all permissions so env.serve_http is absent.
	otelBundle.Manifest.Permissions = nil

	pl, err := rt.LoadPlugin(otelBundle.Manifest.Name, otelBundle.WASMBytes)
	if err != nil {
		t.Fatalf("LoadPlugin: %v", err)
	}
	pl.SetGrants(nil) // no grants

	pp := &PluginPipeline{
		plugins: []*loadedPlugin{{manifest: otelBundle.Manifest, plugin: pl}},
		runtime: rt,
		drained: make(chan struct{}),
		closed:  make(chan struct{}),
	}

	resp, err := pp.RunOnHTTPRequest(context.Background(), 1, "otel", &pb.HttpRequest{
		Method: "GET",
		Path:   "/",
	})
	if !errors.Is(err, ErrServeHTTPForbidden) {
		t.Fatalf("expected ErrServeHTTPForbidden, got resp=%v err=%v", resp, err)
	}
	if resp != nil {
		t.Fatalf("expected nil response on forbidden, got: %+v", resp)
	}
}

// TestRunOnHTTPRequest_ServingPlugin loads the otel plugin with full grants
// and verifies RunOnHTTPRequest returns a 200 HTML response from the plugin's
// run_on_http_request handler.
func TestRunOnHTTPRequest_ServingPlugin(t *testing.T) {
	requireWASM(t, "../../plugins/otel/plugin.wasm")

	pp := newTestPipeline(t, "../../plugins", []string{"otel"})

	resp, err := pp.RunOnHTTPRequest(context.Background(), 1, "otel", &pb.HttpRequest{
		Method: "GET",
		Path:   "/",
	})
	if err != nil {
		t.Fatalf("RunOnHTTPRequest: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response from otel serve_http handler, got nil")
	}
	if !resp.Handled {
		t.Error("expected Handled=true")
	}
	if resp.Status != 200 {
		t.Errorf("expected status 200, got %d", resp.Status)
	}
	if len(resp.Body) == 0 {
		t.Error("expected non-empty body")
	}
}
