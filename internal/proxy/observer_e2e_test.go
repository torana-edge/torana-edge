package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// observerEnv starts a proxy with the test-observer fixture loaded and the
// given upstream handler. Returns a post helper.
func observerEnv(t *testing.T, upstream http.HandlerFunc) func(body string) (int, []byte) {
	t.Helper()
	requireWASM(t, "../../examples/plugins/test-observer/plugin.wasm")

	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)

	srv, err := New(Config{
		Providers: provider.Config{
			Providers: map[string]provider.Provider{"oai": {URL: up.URL, Format: "openai"}},
			Plugins:   provider.PluginsConfig{Dir: "../../examples/plugins", Order: []string{"test-observer"}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 30 * time.Second}

	return func(body string) (int, []byte) {
		req, _ := http.NewRequest("POST", base+"/provider/oai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
}

const observerReq = `{"model":"gpt-x","messages":[{"role":"user","content":"hello"}]}`

// TestObserverSeesResponseSignal: run_after_response receives _response with
// the upstream status and provider-reported usage — the fixture proves it by
// rewriting the assistant content with the observed values.
func TestObserverSeesResponseSignal(t *testing.T) {
	post := observerEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"x","model":"gpt-x","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
	})

	status, body := post(observerReq)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if !strings.Contains(string(body), "observed status=200 in=7 out=3") {
		t.Fatalf("plugin did not receive _response signal; body=%s", body)
	}
}

// TestObserverSeesErrorResponses: upstream errors skip the mutation pipeline,
// but the observe-only hook still fires with _response — the fixture caches
// the observed status and tags the NEXT request's model with it.
func TestObserverSeesErrorResponses(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	post := observerEnv(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		n := len(bodies)
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTeapot) // non-retryable 4xx
			io.WriteString(w, `{"error":{"message":"kaputt"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"x","model":"gpt-x","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	})

	if status, _ := post(observerReq); status != http.StatusTeapot {
		t.Fatalf("first request: status = %d, want 418", status)
	}
	if status, _ := post(observerReq); status != http.StatusOK {
		t.Fatalf("second request: status = %d, want 200", status)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(bodies))
	}
	if !strings.Contains(bodies[1], "+err418") {
		t.Fatalf("error status never reached the plugin (second request untagged): %s", bodies[1])
	}
}

// TestObserverStreamingMutationIsObservational pins the #141 semantics: on a
// STREAMING response, run_after_response mutations are observational — the
// stream has already been written, so the plugin's content rewrite must NOT
// appear in what the client receives. (Contrast TestObserverSeesResponseSignal,
// where the identical plugin's rewrite IS applied on the non-streaming path.)
func TestObserverStreamingMutationIsObservational(t *testing.T) {
	post := observerEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, frame := range []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`,
		} {
			io.WriteString(w, "data: "+frame+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	})

	status, body := post(`{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	// The observer rewrites assistant content to "observed status=..." on the
	// JSON path. On the streaming path that mutation is dropped, so the client
	// gets the original content and never the observed-status string.
	if strings.Contains(string(body), "observed status=") {
		t.Fatalf("#141: run_after_response mutation leaked into the streamed response (should be observational): %s", body)
	}
	if !strings.Contains(string(body), `"content":"hi"`) {
		t.Fatalf("original streamed content missing; body=%s", body)
	}
}
