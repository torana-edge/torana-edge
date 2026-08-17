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

	_ "github.com/torana-edge/torana-edge/internal/format/openai"
	"github.com/torana-edge/torana-edge/internal/provider"
)

// inferenceTestPath keeps cross-format proxy tests honest: each adapter must
// be exercised through an endpoint that the corresponding provider actually
// defines as inference traffic.
func inferenceTestPath(formatName string) string {
	switch formatName {
	case "anthropic":
		return "/v1/messages"
	case "bedrock":
		return "/model/test/converse"
	case "gemini":
		return "/v1beta/models/test:generateContent"
	case "gemini-codeassist":
		return "/v1internal:generateContent"
	default:
		return "/v1/chat/completions"
	}
}

func TestNonInferenceEndpointBypassesIRAndPlugins(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-trapper/plugin.wasm")
	requireWASM(t, "../../examples/plugins/test-trapper-response/plugin.wasm")

	type observed struct {
		method string
		path   string
		query  string
		body   string
		header string
	}
	seen := make(chan observed, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			seen <- observed{body: "read-error: " + err.Error()}
			return
		}
		seen <- observed{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			body:   string(body),
			header: r.Header.Get("X-Vendor-Aux"),
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte("auxiliary-response-not-json"))
	}))
	defer upstream.Close()

	providers := testProviderConfig(upstream.URL, "test", "openai")
	providers.Plugins = provider.PluginsConfig{
		Dir:             "../../examples/plugins",
		Order:           []string{"test-trapper", "test-trapper-response"},
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

	const requestBody = "not-json{SECRET-auxiliary-body"
	req, err := http.NewRequest(http.MethodPost,
		"http://"+ln.Addr().String()+"/provider/test/api/oauth/usage?beta=one&beta=two",
		strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Vendor-Aux", "preserve-me")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status = %d, want upstream %d; an inference hook may have run", resp.StatusCode, http.StatusMultiStatus)
	}
	if got := string(responseBody); got != "auxiliary-response-not-json" {
		t.Fatalf("response = %q, want byte-exact upstream response", got)
	}

	select {
	case got := <-seen:
		want := observed{
			method: http.MethodPost,
			path:   "/api/oauth/usage",
			query:  "beta=one&beta=two",
			body:   requestBody,
			header: "preserve-me",
		}
		if got != want {
			t.Fatalf("upstream request = %+v, want %+v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auxiliary request never reached upstream")
	}

	// Empty-body model discovery is also auxiliary traffic. It must not inherit
	// the inference endpoint's non-empty-body requirement, and the response hook
	// must remain bypassed just like the request hook.
	modelReq, err := http.NewRequest(http.MethodGet,
		"http://"+ln.Addr().String()+"/provider/test/v1/models?limit=10", nil)
	if err != nil {
		t.Fatal(err)
	}
	modelResp, err := client.Do(modelReq)
	if err != nil {
		t.Fatal(err)
	}
	modelBody, readErr := io.ReadAll(modelResp.Body)
	modelResp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if modelResp.StatusCode != http.StatusMultiStatus || string(modelBody) != "auxiliary-response-not-json" {
		t.Fatalf("empty auxiliary response = %d %q, want upstream 207 bytes", modelResp.StatusCode, modelBody)
	}
	select {
	case got := <-seen:
		want := observed{method: http.MethodGet, path: "/v1/models", query: "limit=10"}
		if got != want {
			t.Fatalf("empty auxiliary upstream request = %+v, want %+v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("empty auxiliary request never reached upstream")
	}
}
