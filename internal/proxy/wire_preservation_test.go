package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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
			name:   "bedrock converse",
			format: "bedrock",
			body:   "{ \"messages\" : [ { \"role\" : \"user\", \"content\" : [ { \"text\" : \"hi\" } ] } ], \"inferenceConfig\" : { \"maxTokens\" : 16 }, \"vendor\" : {\"b\":2,\"a\":1} }",
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
