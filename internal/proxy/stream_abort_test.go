package proxy

// Reproduces the former reqState data race on the stream-abort path: when
// RunOnStreamChunkVerified aborts, the pipeline goroutine detaches a drainer
// (`go func() { for range in {} }()`) and returns, closing `out`. The
// serializer goroutine then closes `done`, releasing ServeHTTP to read
// rs.UsageIn/UsageOut — while the usage tap goroutine is still alive and may
// call rs.mergeUsage() for a usage frame that arrives after the abort.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestStreamAbortWaitsForUsageTap(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-tool-rewriter/plugin.wasm")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		frame := func(s string) {
			fmt.Fprint(w, s+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		frame(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hello","thoughtSignature":"SIG_TEXT"}]}}]}`)
		time.Sleep(300 * time.Millisecond)
		// Violating frame -> abort fires here. The usage-bearing finish frames
		// are written in the SAME flush so they are already buffered in the
		// parser when the abort closes the upstream body: the tap goroutine
		// then calls rs.mergeUsage() concurrently with the handler's
		// post-streamDone reads.
		var b strings.Builder
		b.WriteString(`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"search","args":{"q":"original"}},"thoughtSignature":"SIG_CALL"}]}}]}` + "\n\n")
		for i := range 200 {
			b.WriteString(fmt.Sprintf(`data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":%d,"totalTokenCount":168}}`, 100+i, 40+i) + "\n\n")
		}
		frame(strings.TrimSuffix(b.String(), "\n\n"))
	}))
	defer upstream.Close()

	cfg := Config{Port: "0", Providers: provider.Config{
		Providers: map[string]provider.Provider{"gem": {URL: upstream.URL, Format: "gemini"}},
		Plugins: provider.PluginsConfig{
			Dir: fixturesDir, Order: []string{"test-tool-rewriter"}, AllowUnapproved: true,
		},
	}}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	resp, err := http.Post("http://"+ln.Addr().String()+"/provider/gem/v1beta/models/gemini-x:streamGenerateContent",
		"application/json", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("body=%q", body)
	// Give the detached drainer time to run inside the test's lifetime.
	time.Sleep(500 * time.Millisecond)
}
