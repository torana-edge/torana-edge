package plugin

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/torana-edge/torana-edge/internal/wasm"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
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
	requireWASM(t, fixturesDir+"/test-http-server/plugin.wasm")

	rt := wasm.NewRuntime(context.Background())
	defer rt.Close()

	// Load the pipeline with otel but intentionally declare NO permissions
	// so env.serve_http is absent.
	bundles, err := DiscoverPlugins(fixturesDir)
	if err != nil {
		t.Fatalf("DiscoverPlugins: %v", err)
	}
	var httpBundle *PluginBundle
	for i := range bundles {
		if bundles[i].Manifest.Name == "test-http-server" {
			httpBundle = &bundles[i]
			break
		}
	}
	if httpBundle == nil {
		if os.Getenv("TORANA_E2E") != "" {
			t.Fatal("test-http-server fixture not found — run 'make testdata'")
		}
		t.Skip("test-http-server fixture not found")
	}

	// Strip all permissions so env.serve_http is absent.
	httpBundle.Manifest.Permissions = nil

	pl, err := rt.LoadPlugin(httpBundle.Manifest.Name, httpBundle.WASMBytes)
	if err != nil {
		t.Fatalf("LoadPlugin: %v", err)
	}
	pl.SetGrants(nil) // no grants

	pp := &PluginPipeline{
		plugins: []*loadedPlugin{{manifest: httpBundle.Manifest, plugin: pl}},
		runtime: rt,
		drained: make(chan struct{}),
		closed:  make(chan struct{}),
	}

	resp, err := pp.RunOnHTTPRequest(context.Background(), 1, "test-http-server", &pb.HttpRequest{
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
	requireWASM(t, fixturesDir+"/test-http-server/plugin.wasm")

	pp := newTestPipeline(t, fixturesDir, []string{"test-http-server"})

	resp, err := pp.RunOnHTTPRequest(context.Background(), 1, "test-http-server", &pb.HttpRequest{
		Method: "GET",
		Path:   "/",
	})
	if err != nil {
		t.Fatalf("RunOnHTTPRequest: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response from otel serve_http handler, got nil")
	}
	// v2 dropped HttpResponse.Handled: a non-nil response IS the plugin
	// serving the request, and declining is a nil response.
	if resp.Status != 200 {
		t.Errorf("expected status 200, got %d", resp.Status)
	}
	if len(resp.Body) == 0 {
		t.Error("expected non-empty body")
	}
}
