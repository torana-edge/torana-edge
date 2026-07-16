package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// piiEnv starts a proxy with the pii plugin and the given config, an upstream
// that counts hits, and (optionally) extra providers (e.g. a mock local model).
// Returns a post helper and the upstream hit counter.
func piiEnv(t *testing.T, piiCfg string, extra map[string]provider.Provider) (func(body string) (int, []byte), *int32) {
	t.Helper()
	requireWASM(t, "../../plugins/pii/plugin.wasm")

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(upstream.Close)

	providers := map[string]provider.Provider{"oai": {URL: upstream.URL, Format: "openai"}}
	for k, v := range extra {
		providers[k] = v
	}

	srv, err := New(Config{
		Providers: provider.Config{
			Providers: providers,
			Plugins: provider.PluginsConfig{
				Dir:    "../../plugins",
				Order:  []string{"pii"},
				Config: map[string]json.RawMessage{"pii": json.RawMessage(piiCfg)},
			},
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

	post := func(body string) (int, []byte) {
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
	return post, &hits
}

// toolConvo builds an OpenAI request whose history contains one tool result.
func toolConvo(toolContent string) string {
	b, _ := json.Marshal(map[string]any{
		"model": "m",
		"messages": []map[string]any{
			{"role": "user", "content": "run it"},
			{"role": "assistant", "tool_calls": []map[string]any{
				{"id": "call_1", "type": "function", "function": map[string]any{"name": "bash", "arguments": "{}"}},
			}},
			{"role": "tool", "tool_call_id": "call_1", "content": toolContent},
		},
	})
	return string(b)
}

// TestPIIRegexBlock: an email in a tool result is caught by the deterministic
// regex pre-filter (no model needed) → request blocked, error names the type +
// line + tool but NOT the raw value, upstream never called.
func TestPIIRegexBlock(t *testing.T) {
	post, hits := piiEnv(t, `{"tools":["*"],"on_error":"block"}`, nil)
	status, body := post(toolConvo("some notes\ncontact: john.doe@acme.com here"))

	if status != 422 {
		t.Fatalf("status = %d, want 422; body=%s", status, body)
	}
	s := string(body)
	if !strings.Contains(s, "email") || !strings.Contains(s, "line 2") {
		t.Fatalf("error should name type+line: %s", s)
	}
	if !strings.Contains(s, "bash") {
		t.Fatalf("error should name the tool: %s", s)
	}
	if strings.Contains(s, "john.doe@acme.com") {
		t.Fatalf("error LEAKED the raw PII value: %s", s)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("upstream called %d times; blocked request must not reach upstream", n)
	}
}

// TestPIICleanForwards: a clean tool result is forwarded upstream.
func TestPIICleanForwards(t *testing.T) {
	post, hits := piiEnv(t, `{"tools":["*"],"on_error":"block"}`, nil)
	status, body := post(toolConvo("all files compiled successfully, 0 errors"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("upstream called %d times, want 1", n)
	}
}

// TestPIIModelBlock: content the regex misses is sent to the (mock) local model,
// which flags PII → request blocked, upstream not called, no value leaked.
func TestPIIModelBlock(t *testing.T) {
	var gotAuth string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"pii\":true,\"findings\":[{\"type\":\"person_name\",\"line\":1}]}"}}]}`))
	}))
	defer model.Close()

	post, hits := piiEnv(t,
		`{"provider":"local","model":"m1","tools":["*"],"on_error":"block"}`,
		map[string]provider.Provider{"local": {URL: model.URL, Format: "openai"}})

	status, body := post(toolConvo("employee dossier: Jonathan Q. Public, badge 4471, floor 3"))
	if status != 422 {
		t.Fatalf("status = %d, want 422; body=%s", status, body)
	}
	if !strings.Contains(string(body), "person_name") {
		t.Fatalf("error should name the model finding: %s", body)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("upstream called %d times; must be 0", n)
	}
	if gotAuth != "" {
		t.Fatalf("caller credential leaked to local PII model: %q", gotAuth)
	}
}

// TestPIIFailClosed: when the local model errors, the default is to block.
func TestPIIFailClosed(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer model.Close()

	post, hits := piiEnv(t,
		`{"provider":"local","model":"m1","tools":["*"],"on_error":"block"}`,
		map[string]provider.Provider{"local": {URL: model.URL, Format: "openai"}})

	status, body := post(toolConvo("ambiguous content the regex cannot judge"))
	if status != 422 {
		t.Fatalf("fail-closed: status = %d, want 422; body=%s", status, body)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("upstream called %d times; fail-closed must block", n)
	}
}

// TestPIIFailOpen: on_error=allow forwards unscanned when the model is down.
func TestPIIFailOpen(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer model.Close()

	post, hits := piiEnv(t,
		`{"provider":"local","model":"m1","tools":["*"],"on_error":"allow"}`,
		map[string]provider.Provider{"local": {URL: model.URL, Format: "openai"}})

	status, _ := post(toolConvo("ambiguous content the regex cannot judge"))
	if status != http.StatusOK {
		t.Fatalf("fail-open: status = %d, want 200", status)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("upstream called %d times, want 1 (fail-open forwards)", n)
	}
}
