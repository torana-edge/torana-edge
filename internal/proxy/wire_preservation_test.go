package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// TestUnchangedInferenceRequestPreservesProviderWire pins the transparent
// half of the inference boundary. Torana still validates and projects each
// known provider request into the canonical IR, but an observational request
// with no provider-visible mutation reaches upstream byte-for-byte. This is
// deliberately cross-format: a harness must not pay a normalization tax merely
// for pointing its inference endpoint at Torana.
func TestUnchangedInferenceRequestPreservesProviderWire(t *testing.T) {
	rows := []struct {
		name   string
		format string
		body   string
		order  []string
	}{
		{
			name:   "openai chat",
			format: "openai",
			body:   "{ \n  \"model\" : \"gpt-x\", \"messages\" : [ { \"role\" : \"user\", \"content\" : \"hi\" } ], \"vendor\" : {\"n\":1.0,\"large\":9007199254740993} }\n",
		},
		{
			name:   "anthropic",
			format: "anthropic",
			body:   "{\"model\":\"claude-x\", \"max_tokens\":16, \"messages\":[{\"role\":\"user\",\"content\":\"hi\"}], \"vendor\":{\"z\":1e3,\"a\":1.0}}",
		},
		{
			name:   "pass-only request hook",
			format: "openai",
			body:   "{ \"model\" : \"gpt-pass\", \"messages\" : [ { \"role\" : \"user\", \"content\" : \"hi\" } ], \"vendor\" : {\"lexeme\":1.0} }",
			order:  []string{"test-observer"},
		},
		{
			name:   "gemini",
			format: "gemini",
			body:   "{\"contents\":[{\"role\":\"user\",\"parts\":[{\"text\":\"hi\"}]}], \"generationConfig\": {\"maxOutputTokens\":16, \"vendor\":1.0}}",
		},
		{
			name:   "gemini code assist",
			format: "gemini-codeassist",
			body:   "{ \"model\" : \"gemini-x\", \"outerExtra\" : {\"n\":1.0}, \"request\" : { \"innerExtra\" : {\"large\":9007199254740993}, \"contents\" : [ { \"role\" : \"user\", \"parts\" : [ { \"text\" : \"hi\" } ] } ] } }",
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			for _, name := range row.order {
				requireWASM(t, "../../examples/plugins/"+name+"/plugin.wasm")
			}
			seen := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					seen <- nil
				} else {
					seen <- b
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTeapot)
				_, _ = io.WriteString(w, `{"error":{"message":"stop after capture"}}`)
			}))
			t.Cleanup(upstream.Close)

			provCfg := provider.Config{Providers: map[string]provider.Provider{
				"p": {URL: upstream.URL, Format: row.format},
			}}
			if len(row.order) > 0 {
				provCfg.Plugins = provider.PluginsConfig{
					Dir:             "../../examples/plugins",
					Order:           row.order,
					AllowUnapproved: true,
				}
			}
			srv, err := New(Config{Providers: provCfg})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			proxy := httptest.NewServer(srv.Handler())
			t.Cleanup(func() {
				proxy.Close()
				_ = srv.Shutdown(context.Background())
			})

			req, err := http.NewRequest(http.MethodPost,
				proxy.URL+"/provider/p"+inferenceTestPath(row.format),
				bytes.NewBufferString(row.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			got := <-seen
			if !bytes.Equal(got, []byte(row.body)) {
				t.Fatalf("upstream body changed\n got: %q\nwant: %q", got, row.body)
			}
		})
	}
}

func TestGzipInferenceRequestIsBoundedlyDecodedForAllNativeFormats(t *testing.T) {
	rows := []struct{ name, format, path, body string }{
		{"openai-chat", "openai", "/v1/chat/completions", `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`},
		{"openai-responses", "openai", "/v1/responses", `{"model":"gpt-x","input":"hi"}`},
		{"anthropic", "anthropic", "/v1/messages", `{"model":"claude-x","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`},
		{"gemini", "gemini", "/v1beta/models/gemini-x:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
		{"gemini-codeassist", "gemini-codeassist", "/v1internal:generateContent", `{"model":"gemini-x","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			seen := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != row.path {
					t.Errorf("upstream path = %q, want %q", r.URL.Path, row.path)
				}
				if got := r.Header.Get("Content-Encoding"); got != "" {
					t.Errorf("upstream Content-Encoding = %q", got)
				}
				got, _ := io.ReadAll(r.Body)
				seen <- got
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
			}))
			defer upstream.Close()
			srv, err := New(Config{Providers: provider.Config{Providers: map[string]provider.Provider{
				"p": {URL: upstream.URL, Format: row.format},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(srv.Handler())
			defer func() { proxy.Close(); _ = srv.Shutdown(context.Background()) }()

			var compressed bytes.Buffer
			zw := gzip.NewWriter(&compressed)
			_, _ = zw.Write([]byte(row.body))
			_ = zw.Close()
			req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/provider/p"+row.path, bytes.NewReader(compressed.Bytes()))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Content-Encoding", "gzip")
			resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			select {
			case got := <-seen:
				if !bytes.Equal(got, []byte(row.body)) {
					t.Fatalf("upstream got %q, want decoded %q", got, row.body)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for decoded request to reach upstream")
			}
		})
	}
}

func TestGzipAuxiliaryRequestRemainsOpaque(t *testing.T) {
	original := []byte("opaque compressed account payload")
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = zw.Write(original)
	_ = zw.Close()
	encoded := append([]byte(nil), compressed.Bytes()...)
	seen := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Encoding"); got != "gzip" {
			t.Errorf("Content-Encoding = %q, want gzip", got)
		}
		got, _ := io.ReadAll(r.Body)
		seen <- got
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	srv, err := New(Config{Providers: provider.Config{Providers: map[string]provider.Provider{
		"p": {URL: upstream.URL, Format: "openai"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer func() { proxy.Close(); _ = srv.Shutdown(context.Background()) }()
	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/provider/p/v1/account", bytes.NewReader(encoded))
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := <-seen; !bytes.Equal(got, encoded) {
		t.Fatal("auxiliary compressed body changed")
	}
}

func TestInvalidOrOversizedGzipInferenceRequestNeverReachesUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	srv, err := New(Config{Providers: provider.Config{Providers: map[string]provider.Provider{
		"p": {URL: upstream.URL, Format: "openai"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer func() { proxy.Close(); _ = srv.Shutdown(context.Background()) }()

	var oversized bytes.Buffer
	zw := gzip.NewWriter(&oversized)
	_, _ = zw.Write(bytes.Repeat([]byte("x"), maxBodySize+1))
	_ = zw.Close()
	for name, encoded := range map[string][]byte{
		"invalid":   []byte("not-gzip"),
		"oversized": oversized.Bytes(),
	} {
		t.Run(name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/provider/p/v1/responses", bytes.NewReader(encoded))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Content-Encoding", "gzip")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400", resp.StatusCode)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("malformed compressed requests reached upstream %d times", calls.Load())
	}
}

func TestProviderVisiblePluginReplacementForcesMarshal(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-records-invocation/plugin.wasm")

	seen := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen <- b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, `{"error":{"message":"stop after capture"}}`)
	}))
	t.Cleanup(upstream.Close)

	provCfg := provider.Config{
		Providers: map[string]provider.Provider{"p": {URL: upstream.URL, Format: "openai"}},
		Plugins: provider.PluginsConfig{
			Dir:             "../../examples/plugins",
			Order:           []string{"test-records-invocation"},
			AllowUnapproved: true,
		},
	}
	srv, err := New(Config{Providers: provCfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		proxy.Close()
		_ = srv.Shutdown(context.Background())
	})

	const body = `{ "model" : "gpt-mutate", "messages" : [ { "role" : "user", "content" : "hi" } ], "vendor" : {"n":1.0} }`
	req, _ := http.NewRequest(http.MethodPost,
		proxy.URL+"/provider/p/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	got := <-seen
	if bytes.Equal(got, []byte(body)) {
		t.Fatal("provider-visible plugin replacement incorrectly reused the original wire body")
	}
	var wire struct {
		Model  string          `json:"model"`
		Vendor json.RawMessage `json:"vendor"`
	}
	// The OpenAI adapter deliberately preserves provider extensions, so decode
	// the complete wire through a map after pinning the two relevant members.
	var complete map[string]json.RawMessage
	if err := json.Unmarshal(got, &complete); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal(complete["model"], &wire.Model); err != nil {
		t.Fatalf("model: %v", err)
	}
	wire.Vendor = complete["vendor"]
	if wire.Model != "gpt-mutate+downstream-ran" {
		t.Fatalf("model = %q, replacement did not reach upstream", wire.Model)
	}
	if !bytes.Equal(wire.Vendor, []byte(`{"n":1.0}`)) {
		t.Fatalf("provider extension changed: %s", wire.Vendor)
	}
}
