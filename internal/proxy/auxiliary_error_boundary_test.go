package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

	// And it must not have entered inference accounting at all. Auxiliary
	// traffic is transparent reverse-proxy traffic: it never reached the IR,
	// so it has no place in the live feed an operator reads as the inference
	// record. An earlier version of this check asserted the weaker shape — a
	// feed entry listing no plugins — which a blank, unattributable row would
	// have satisfied.
	for _, ev := range srv.feed.Snapshot() {
		t.Errorf("auxiliary %d produced a feed entry (provider=%q model=%q plugins=%v); "+
			"docs/HARNESS_COMPATIBILITY.md promises non-inference traffic stays out "+
			"of the pipeline", resp.StatusCode, ev.Provider, ev.RequestedModel, ev.Plugins)
	}

	// Positive control, so an empty feed cannot pass this test vacuously: the
	// SAME upstream, the SAME 401, on a path that IS inference. That one must
	// be recorded, and must name the plugin that ran on it.
	infReq, err := http.NewRequest(http.MethodPost,
		"http://"+ln.Addr().String()+"/provider/test/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	infReq.Header.Set("Content-Type", "application/json")
	infResp, err := client.Do(infReq)
	if err != nil {
		t.Fatalf("POST chat/completions: %v", err)
	}
	infResp.Body.Close()

	events := srv.feed.Snapshot()
	if len(events) != 1 {
		t.Fatalf("the feed holds %d entries after one auxiliary and one inference "+
			"request, want exactly the inference one; this check cannot tell a real "+
			"bypass from a feed that never records anything", len(events))
	}
	if len(events[0].Plugins) == 0 {
		t.Error("the inference request recorded no plugins, so the pipeline was not " +
			"running and the auxiliary assertion above proves nothing")
	}
}
