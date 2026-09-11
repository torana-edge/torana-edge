package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// Torana promises that auxiliary provider traffic stays outside the plugin
// pipeline. docs/HARNESS_COMPATIBILITY.md states it without qualification —
// "Neither request nor response WASM hooks run" — and README.md says "Only
// recognized inference calls enter the IR and plugin pipeline". An operator
// approving a plugin's env.file_append budget is relying on that: the plugin
// is supposed to see inference traffic, not every account, quota and
// model-discovery call the harness makes.
//
// The promise held only while the upstream answered successfully. The
// ModifyResponse error branch ran run_after_response for ANY response with
// status >= 400, with no check that the request was ever recognized as
// inference — while the success branch below it is correctly gated on the
// request having a resolved format.
//
// The existing boundary tests could not catch it: they all answer 207. This
// one answers 401, which is the ordinary outcome of the model-discovery call
// the quickstart tells a new user to run before they have a working key.
func TestAuxiliaryErrorResponsesStayOutOfThePluginPipeline(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-trapper-response/plugin.wasm")

	const upstreamBody = `{"error":{"message":"auxiliary auth failure","type":"authentication_error"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// JSON, so the response cannot be dismissed as an unrecognised media
		// type — this is what a real provider returns for an unauthenticated
		// model-discovery call.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	providers := testProviderConfig(upstream.URL, "test", "openai")
	providers.Plugins = provider.PluginsConfig{
		Dir:             "../../examples/plugins",
		Order:           []string{"test-trapper-response"},
		AllowUnapproved: true,
	}
	srv, err := New(Config{Port: "0", Providers: providers})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + ln.Addr().String() + "/provider/test/v1/models")
	if err != nil {
		t.Fatalf("GET models: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}

	// The upstream's own answer, byte for byte.
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want the upstream's %d", resp.StatusCode, http.StatusUnauthorized)
	}
	if string(body) != upstreamBody {
		t.Errorf("body = %q, want the upstream's %q", body, upstreamBody)
	}

	// And no plugin may have been invoked. This is the operator-visible
	// signal: the control-plane feed names the plugins that fired, so a
	// plugin listed here is a plugin the operator is told ran.
	events := srv.feed.Snapshot()
	if len(events) == 0 {
		t.Fatal("no request reached the feed, so this check proves nothing")
	}
	if got := events[0].Plugins; len(got) != 0 {
		t.Errorf("auxiliary %d ran plugins %v; docs/HARNESS_COMPATIBILITY.md promises "+
			"neither request nor response WASM hooks run on non-inference traffic",
			resp.StatusCode, got)
	}
}
