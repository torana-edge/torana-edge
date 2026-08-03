package proxy

// End-to-end tests for the approved known-format parse-bypass seam: a body
// that a KNOWN CONFIGURED format cannot parse is a host input-validation
// failure — a value-free, provider-native HTTP 400 short-circuits BEFORE rate
// limiting and upstream, identical whether plugins are loaded or not, and
// independent of plugin failure_mode (no valid IR exists, so no request hook
// runs). Empty configured format and empty-body passthrough remain
// intentional; an unknown nonempty format remains a config-time error.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// logSink captures std log output so the value-free logging contract can be
// asserted.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureLogs(t *testing.T) *logSink {
	t.Helper()
	sink := &logSink{}
	prev := log.Writer()
	log.SetOutput(sink)
	t.Cleanup(func() { log.SetOutput(prev) })
	return sink
}

// parseFailE2E spins up a proxy with a provider in the given format (and an
// optional plugin order) against a hit-counting upstream, and POSTs the body
// to the provider path.
func parseFailE2E(t *testing.T, format string, order []string, body string) (int, []byte, *int32) {
	t.Helper()
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(upstream.Close)

	provCfg := provider.Config{
		Providers: map[string]provider.Provider{"p": {URL: upstream.URL, Format: format}},
	}
	if len(order) > 0 {
		requireWASM(t, "../../examples/plugins/"+order[0]+"/plugin.wasm")
		provCfg.Plugins = provider.PluginsConfig{Dir: "../../examples/plugins", Order: order, AllowUnapproved: true}
	}
	srv, err := New(Config{Port: "0", Providers: provCfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	client := &http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/provider/p/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, &hits
}

// TestParseFailClosedPerFormat: every configured format returns its
// provider-native 400 shape for a body it cannot parse, with zero upstream
// calls.
func TestParseFailClosedPerFormat(t *testing.T) {
	const malformed = `not-json{`
	cases := map[string]func(m map[string]any) bool{
		"anthropic": func(m map[string]any) bool {
			if m["type"] != "error" {
				return false
			}
			e, _ := m["error"].(map[string]any)
			return e != nil && e["type"] == "invalid_request_error" &&
				strings.Contains(e["message"].(string), "anthropic")
		},
		"openai": func(m map[string]any) bool {
			e, _ := m["error"].(map[string]any)
			return e != nil && e["type"] == "invalid_request_error" &&
				strings.Contains(e["message"].(string), "openai")
		},
		"gemini": func(m map[string]any) bool {
			e, _ := m["error"].(map[string]any)
			return e != nil && e["code"].(float64) == 400 && e["status"] == "INVALID_ARGUMENT" &&
				strings.Contains(e["message"].(string), "gemini")
		},
		"gemini-codeassist": func(m map[string]any) bool {
			e, _ := m["error"].(map[string]any)
			return e != nil && e["code"].(float64) == 400 && e["status"] == "INVALID_ARGUMENT"
		},
		"bedrock": func(m map[string]any) bool {
			s, _ := m["message"].(string)
			return strings.Contains(s, "bedrock")
		},
	}
	for format, check := range cases {
		t.Run(format, func(t *testing.T) {
			status, body, hits := parseFailE2E(t, format, nil, malformed)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", status, body)
			}
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatalf("body not JSON: %v (%s)", err, body)
			}
			if !check(m) {
				t.Fatalf("%s envelope wrong: %s", format, body)
			}
			if n := atomic.LoadInt32(hits); n != 0 {
				t.Fatalf("upstream was called %d times; a malformed known-format body must never reach upstream", n)
			}
		})
	}
}

// TestParseFailClosedPluginPresenceAndFailureMode: the 400 is IDENTICAL with a
// block-mode plugin loaded (test-trapper traps every request) and with no
// plugins — the parse failure happens before any hook can run, so neither
// plugin presence nor failure_mode can change it, and the body must never
// mention plugin_failure.
func TestParseFailClosedPluginPresenceAndFailureMode(t *testing.T) {
	withPlugins, bodyWith, hitsWith := parseFailE2E(t, "anthropic", []string{"test-trapper"}, `not-json{`)
	withoutPlugins, bodyWithout, hitsWithout := parseFailE2E(t, "anthropic", nil, `not-json{`)
	if withPlugins != http.StatusBadRequest || withoutPlugins != http.StatusBadRequest {
		t.Fatalf("status with plugins = %d, without = %d; both must be 400", withPlugins, withoutPlugins)
	}
	if !bytes.Equal(bodyWith, bodyWithout) {
		t.Fatalf("bodies differ with/without plugins:\nwith:    %s\nwithout: %s", bodyWith, bodyWithout)
	}
	if strings.Contains(string(bodyWith), "plugin_failure") {
		t.Fatalf("body mentions plugin_failure: %s", bodyWith)
	}
	if n := atomic.LoadInt32(hitsWith); n != 0 {
		t.Fatalf("upstream called %d times with plugins loaded; must be 0", n)
	}
	if n := atomic.LoadInt32(hitsWithout); n != 0 {
		t.Fatalf("upstream called %d times without plugins; must be 0", n)
	}
}

// TestParseFailClosedValueFree: a malformed body carrying a secret gets a 400
// whose body AND the captured server log never contain the secret or any raw
// request data.
func TestParseFailClosedValueFree(t *testing.T) {
	const secret = "sk-ant-a1b2c3d4e5f6"
	body := `{"model":"m","secret_key":"` + secret + `","messages":[{"role":"user","content":5}]}`
	sink := captureLogs(t)
	status, respBody, hits := parseFailE2E(t, "anthropic", nil, body)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, respBody)
	}
	for name, data := range map[string]string{"response body": string(respBody), "server log": sink.String()} {
		if strings.Contains(data, secret) {
			t.Fatalf("%s contains the request secret", name)
		}
		if strings.Contains(data, "sk-ant") {
			t.Fatalf("%s contains raw request data", name)
		}
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("upstream called %d times; must be 0", n)
	}
}

// TestParseFailClosedStringSystemAccepted: a VALID request using the string
// `system` form is accepted by the host and forwarded upstream (200, one
// upstream call). The bundle-gated PII row for this shape lands with unit 5.
func TestParseFailClosedStringSystemAccepted(t *testing.T) {
	body := `{"model":"m","system":"You are a coding agent.","messages":[{"role":"user","content":"hi"}]}`
	status, respBody, hits := parseFailE2E(t, "anthropic", nil, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (string system must parse); body=%s", status, respBody)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}
}

// TestParseFailClosedEmptyBodyPassthrough: an empty body on a configured
// format (e.g. GET-style model-list requests) stays an intentional
// transparent pass-through.
func TestParseFailClosedEmptyBodyPassthrough(t *testing.T) {
	status, _, hits := parseFailE2E(t, "anthropic", nil, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (empty body must pass through)", status)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}
}

// TestParseFailClosedEmptyFormatPassthrough: a provider with no format is the
// deliberate transparent pass-through mode — even a garbage body is
// forwarded untouched.
func TestParseFailClosedEmptyFormatPassthrough(t *testing.T) {
	status, _, hits := parseFailE2E(t, "", nil, `not-json{`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (empty format must pass through)", status)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}
}

// TestUnknownNonemptyFormatRejectedAtConfigTime: an unknown NONEMPTY format is
// rejected when the configuration validates — it is a config-time error, not
// a request-time passthrough. This is the same Validate the control plane
// runs on reload.
func TestUnknownNonemptyFormatRejectedAtConfigTime(t *testing.T) {
	cfg := provider.Config{
		Port:      8080,
		Providers: map[string]provider.Provider{"p": {URL: "http://127.0.0.1:1", Format: "klingon"}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "unsupported format") {
		t.Fatalf("err = %v, want an unsupported-format config-time rejection", err)
	}
}
