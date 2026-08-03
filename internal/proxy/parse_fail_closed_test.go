package proxy

// End-to-end tests for the approved known-format parse-bypass seam: a body
// that a KNOWN CONFIGURED format cannot parse is a host input-validation
// failure — a value-free, provider-native HTTP 400 short-circuits BEFORE rate
// limiting and upstream, identical whether plugins are loaded or not, and
// independent of plugin failure_mode. The response is fully host-local
// (Synthetic): no upstream status is recorded and no response hook runs. No
// request hook runs either — no valid IR exists. Empty configured format and
// empty-body passthrough remain intentional; an unknown nonempty format
// remains a config-time error.

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/plugin"
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
// to the provider path. Rate limits are ENABLED (Concurrency: 8) so a limiter
// acquisition would materialize a bucket the test can observe; a fail-closed
// request must never materialize one.
func parseFailE2E(t *testing.T, format string, order []string, body string) (int, []byte, *int32, *Server) {
	t.Helper()
	return parseFailE2EUpstream(t, format, order, body, http.StatusOK)
}

// parseFailE2EUpstream is parseFailE2E with a configurable upstream status,
// for rows that must exercise the genuine upstream-error hook path.
func parseFailE2EUpstream(t *testing.T, format string, order []string, body string, upstreamStatus int) (int, []byte, *int32, *Server) {
	t.Helper()
	return parseFailE2EApproved(t, format, order, body, upstreamStatus, "")
}

// parseFailE2EApproved is parseFailE2EUpstream with an explicit approval
// failure-mode override for the first plugin in order, so a block-mode
// fixture can be forced to allow (or vice versa) for the failure-mode
// independence rows.
func parseFailE2EApproved(t *testing.T, format string, order []string, body string, upstreamStatus int, approvalFailureMode string) (int, []byte, *int32, *Server) {
	t.Helper()
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(upstreamStatus)
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(upstream.Close)

	provCfg := provider.Config{
		Providers: map[string]provider.Provider{"p": {URL: upstream.URL, Format: format}},
		Limits:    provider.Limits{Concurrency: 8},
	}
	if len(order) > 0 {
		requireWASM(t, "../../examples/plugins/"+order[0]+"/plugin.wasm")
		if approvalFailureMode != "" {
			digest, err := plugin.BundleDigestForDir("../../examples/plugins/" + order[0])
			if err != nil {
				t.Fatalf("BundleDigestForDir: %v", err)
			}
			provCfg.Plugins = provider.PluginsConfig{
				Dir:   "../../examples/plugins",
				Order: order,
				Approvals: map[string]provider.PluginApproval{
					order[0]: {Digest: digest, FailureMode: approvalFailureMode},
				},
			}
		} else {
			provCfg.Plugins = provider.PluginsConfig{Dir: "../../examples/plugins", Order: order, AllowUnapproved: true}
		}
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
	return resp.StatusCode, b, &hits, srv
}

// limiterBucketCount returns the number of materialized limiter buckets.
func limiterBucketCount(srv *Server) int {
	srv.rateLimiter.mu.Lock()
	defer srv.rateLimiter.mu.Unlock()
	return len(srv.rateLimiter.limits)
}

// logHeaderRe matches the std log line format followed by exactly the
// sanitized rejection message: <timestamp> format <name>: rejecting malformed
// request body.
var logHeaderRe = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} format anthropic: rejecting malformed request body$`)

// goldenInvalidRequest is the byte-exact provider-native 400 envelope each
// format must receive for a body it cannot parse. Hard-coded INDEPENDENTLY of
// renderInvalidRequest: if production and the test drift together, this fails.
func goldenInvalidRequest(format string) string {
	switch format {
	case "anthropic":
		return `{"error":{"message":"the request body could not be parsed as a valid anthropic request","type":"invalid_request_error"},"type":"error"}`
	case "openai":
		return `{"error":{"code":"invalid_request_error","message":"the request body could not be parsed as a valid openai request","type":"invalid_request_error"}}`
	case "gemini":
		return `{"error":{"code":400,"message":"the request body could not be parsed as a valid gemini request","status":"INVALID_ARGUMENT"}}`
	case "gemini-codeassist":
		return `{"error":{"code":400,"message":"the request body could not be parsed as a valid gemini-codeassist request","status":"INVALID_ARGUMENT"}}`
	case "bedrock":
		return `{"message":"the request body could not be parsed as a valid bedrock request"}`
	}
	panic("unknown format " + format)
}

// TestParseFailClosedMatrix: for EVERY configured format, four body
// categories — malformed syntax, truncated document, wrong-typed required
// member, and a syntactically valid adversarial shape the adapter must reject
// — each yield the byte-exact provider-native 400, zero limiter acquisition,
// zero upstream calls, and no after-response hook execution (observer cache
// stays empty).
func TestParseFailClosedMatrix(t *testing.T) {
	formats := []string{"anthropic", "openai", "gemini", "gemini-codeassist", "bedrock"}
	bodies := map[string][]string{
		"anthropic": {
			"not-json{",
			`{"model":"m","messages":[`,
			`{"model":"m","messages":[{"role":"user","content":5}]}`,
			`{"model":"m","system":5,"messages":[{"role":"user","content":"hi"}]}`,
			`{"model":"m","system":null,"messages":[{"role":"user","content":"hi"}]}`,
		},
		"openai": {
			"not-json{",
			`{"model":"m","messages":[`,
			`{"model":"m","messages":[{"role":5,"content":"hi"}]}`,
			`{"model":"m","messages":"nope"}`,
		},
		"gemini": {
			"not-json{",
			`{"contents":[`,
			`{"contents":5}`,
			`{"contents":[{"role":"user","parts":{"text":"hi"}}]}`,
		},
		"gemini-codeassist": {
			"not-json{",
			`{"request":{"contents":[`,
			`{"request":{"contents":5}}`,
			`{"request":{"contents":"nope"}}`,
		},
		"bedrock": {
			"not-json{",
			`{"modelId":"m","messages":[`,
			`{"modelId":"m","messages":[{"role":"user","content":5}]}`,
			`{"modelId":"m","messages":"nope"}`,
		},
	}
	for _, format := range formats {
		for i, body := range bodies[format] {
			t.Run(format+"/row"+string(rune('a'+i)), func(t *testing.T) {
				status, respBody, hits, srv := parseFailE2E(t, format, nil, body)
				if status != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400; body=%s", status, respBody)
				}
				if got := string(respBody); got != goldenInvalidRequest(format) {
					t.Fatalf("envelope differs from the golden bytes:\ngot:  %s\nwant: %s", got, goldenInvalidRequest(format))
				}
				if n := atomic.LoadInt32(hits); n != 0 {
					t.Fatalf("upstream was called %d times; a malformed known-format body must never reach upstream", n)
				}
				if n := limiterBucketCount(srv); n != 0 {
					t.Fatalf("%d limiter buckets materialized; the fail-closed 400 must not acquire the limiter", n)
				}
			})
		}
	}
}

// TestParseFailClosedPluginPresenceAndFailureMode: the 400 is byte-IDENTICAL
// with a block-mode plugin loaded (test-trapper traps every request), with an
// allow-mode observer loaded, and with no plugins. The parse failure happens
// before any hook can run, so neither plugin presence nor failure_mode can
// change it, and the body must never mention plugin_failure.
func TestParseFailClosedPluginPresenceAndFailureMode(t *testing.T) {
	withBlock, bodyBlock, hitsBlock, _ := parseFailE2E(t, "anthropic", []string{"test-trapper"}, `not-json{`)
	withAllow, bodyAllow, hitsAllow, _ := parseFailE2E(t, "anthropic", []string{"test-observer"}, `not-json{`)
	without, bodyWithout, hitsWithout, _ := parseFailE2E(t, "anthropic", nil, `not-json{`)
	for name, got := range map[string]int{"block": withBlock, "allow": withAllow, "none": without} {
		if got != http.StatusBadRequest {
			t.Fatalf("status with %s plugins = %d, want 400", name, got)
		}
	}
	if !bytes.Equal(bodyBlock, bodyWithout) || !bytes.Equal(bodyAllow, bodyWithout) {
		t.Fatalf("bodies differ across plugin configurations:\nblock: %s\nallow: %s\nnone:  %s", bodyBlock, bodyAllow, bodyWithout)
	}
	if strings.Contains(string(bodyBlock), "plugin_failure") {
		t.Fatalf("body mentions plugin_failure: %s", bodyBlock)
	}
	for name, n := range map[string]int32{"block": atomic.LoadInt32(hitsBlock), "allow": atomic.LoadInt32(hitsAllow), "none": atomic.LoadInt32(hitsWithout)} {
		if n != 0 {
			t.Fatalf("upstream called %d times with %s plugins; must be 0", n, name)
		}
	}
}

// TestParseFailClosedNoHooksNoUpstreamFacts: the local 400 is a complete
// host-local response. (1) Channel validity: with test-observer loaded, a
// genuine upstream 500 runs its after-response hook, which caches the
// observed upstream status — proving the cache channel works. (2) The
// malformed-body 400 then must NOT run the after-response hook (cache absent)
// or record an upstream-status fact, under BOTH failure modes: with
// test-observer (pass) and with test-trapper-response (block — if the hook
// ran, the response would be replaced by a 502 plugin_failure).
func TestParseFailClosedNoHooksNoUpstreamFacts(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-observer/plugin.wasm")

	// (1) Channel validity: upstream 500 on a VALID body runs the observer's
	// after-response hook, which caches the observed upstream status.
	status, _, _, srv := parseFailE2EUpstream(t, "anthropic", []string{"test-observer"},
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, http.StatusInternalServerError)
	if status != http.StatusInternalServerError {
		t.Fatalf("channel-validity status = %d, want 500", status)
	}
	store := srv.sharedCache
	if v, ok := store.Get("observed_error_status"); !ok || v != "500" {
		t.Fatalf("channel validity: observer cache = %q (present %v), want 500 — the hook must run for a real upstream error", v, ok)
	}

	// (2) allow mode: the local 400 must not run the hook (cache absent) and
	// must not materialize an upstream-status fact (no limiter bucket, no
	// upstream call, and the response is the host-local golden body).
	status, body, hits, srvAllow := parseFailE2E(t, "anthropic", []string{"test-observer"}, `not-json{`)
	if status != http.StatusBadRequest || string(body) != goldenInvalidRequest("anthropic") {
		t.Fatalf("allow-mode local 400 wrong: status=%d body=%s", status, body)
	}
	if v, ok := srvAllow.sharedCache.Get("observed_error_status"); ok {
		t.Fatalf("after-response hook ran on the local 400: cache %q", v)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("upstream called %d times; must be 0", n)
	}
	if n := limiterBucketCount(srvAllow); n != 0 {
		t.Fatalf("%d limiter buckets materialized; the fail-closed 400 must not acquire the limiter", n)
	}

	// (3) block mode: if any after-response hook ran, failure_mode block
	// would swap the body for a 502 plugin_failure.
	status, body, hits, _ = parseFailE2E(t, "anthropic", []string{"test-trapper-response"}, `not-json{`)
	if status != http.StatusBadRequest || string(body) != goldenInvalidRequest("anthropic") {
		t.Fatalf("block-mode local 400 wrong: status=%d body=%s (a hook ran: failure_mode block replaced the host body)", status, body)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("upstream called %d times with the block-mode plugin; must be 0", n)
	}
}

// TestParseFailClosedValueFree: a malformed body carrying BOTH a unique
// non-secret marker and a secret gets a 400 whose body AND the captured
// server log never contain the marker, the secret, or any raw request data;
// the sanitized log line is asserted exactly.
func TestParseFailClosedValueFree(t *testing.T) {
	const (
		marker = "marker-7f3a9c21"
		secret = "sk-ant-a1b2c3d4e5f6"
	)
	body := `{"model":"m","unique_marker":"` + marker + `","secret_key":"` + secret + `","messages":[{"role":"user","content":5}]}`
	sink := captureLogs(t)
	status, respBody, hits, _ := parseFailE2E(t, "anthropic", nil, body)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, respBody)
	}
	logs := sink.String()
	for name, data := range map[string]string{"response body": string(respBody), "server log": logs} {
		if strings.Contains(data, marker) || strings.Contains(data, secret) || strings.Contains(data, "sk-ant") {
			t.Fatalf("%s contains request data (marker/secret)", name)
		}
		if strings.Contains(data, `"unique_marker"`) || strings.Contains(data, "unique_marker") {
			t.Fatalf("%s contains a raw-body fragment", name)
		}
	}
	// Exactly ONE sanitized rejection line for the request, structurally
	// exact: the standard log header timestamp followed ONLY by the
	// format-named rejection — no adapter error, no body data. Counting makes
	// the assertion non-vacuous: a missing line fails just like a wrong one.
	rejectionLines := 0
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "rejecting malformed request body") {
			rejectionLines++
			if !logHeaderRe.MatchString(line) {
				t.Fatalf("sanitized log line unexpected: %q", line)
			}
		}
	}
	if rejectionLines != 1 {
		t.Fatalf("sanitized rejection lines = %d, want exactly 1", rejectionLines)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("upstream called %d times; must be 0", n)
	}
}

// TestParseFailClosedStringSystemAccepted: a VALID request using the string
// `system` form is accepted by the host and forwarded upstream (200, one
// upstream call). The bundle-gated PII row for this shape lives in
// pii_anthropic_e2e_test.go.
func TestParseFailClosedStringSystemAccepted(t *testing.T) {
	body := `{"model":"m","system":"You are a coding agent.","messages":[{"role":"user","content":"hi"}]}`
	status, respBody, hits, _ := parseFailE2E(t, "anthropic", nil, body)
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
	status, _, hits, _ := parseFailE2E(t, "anthropic", nil, "")
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
	status, _, hits, _ := parseFailE2E(t, "", nil, `not-json{`)
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

// TestParseFailClosedParserDifferentialHazards: JSON-text hazards that
// encoding/json silently normalizes (duplicate keys incl. escape-equivalent
// names, invalid UTF-8, lone surrogates) are rejected BEFORE the adapter for
// every configured format, with the same golden 400, zero limiter buckets,
// and zero upstream calls. The validator is shared code; anthropic and openai
// get the full set, gemini and bedrock one row each.
func TestParseFailClosedParserDifferentialHazards(t *testing.T) {
	hazards := []string{
		`{"messages":[1],"messages":[2]}`,
		`{"messages":[1],"\u006d\u0065ssages":[2]}`,
		`{"a":{"b":1,"b":2}}`,
		"{\"a\":\"\xff\xfe\"}",
		`{"a":"\ud800"}`,
		`{"a":"\udc00"}`,
	}
	full := map[string][]string{
		"anthropic": hazards,
		"openai":    hazards,
		"gemini":    hazards[:1],
		"bedrock":   hazards[:1],
	}
	for format, rows := range full {
		for i, body := range rows {
			t.Run(format+"/hazard"+string(rune('a'+i)), func(t *testing.T) {
				status, respBody, hits, srv := parseFailE2E(t, format, nil, body)
				if status != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400; body=%s", status, respBody)
				}
				if got := string(respBody); got != goldenInvalidRequest(format) {
					t.Fatalf("envelope differs from the golden bytes:\ngot:  %s\nwant: %s", got, goldenInvalidRequest(format))
				}
				if n := atomic.LoadInt32(hits); n != 0 {
					t.Fatalf("upstream called %d times; a hazard body must never reach upstream", n)
				}
				if n := limiterBucketCount(srv); n != 0 {
					t.Fatalf("%d limiter buckets materialized; the fail-closed 400 must not acquire the limiter", n)
				}
			})
		}
	}
}

// TestParseFailClosedHazardPositiveControls: a valid surrogate pair and a
// literal escaped backslash-u sequence are NOT hazards — they must reach
// upstream normally.
func TestParseFailClosedHazardPositiveControls(t *testing.T) {
	for name, body := range map[string]string{
		"surrogate pair":      `{"model":"m","system":[{"type":"text","text":"\ud83d\ude00"}],"messages":[{"role":"user","content":"hi"}]}`,
		"literal backslash-u": `{"model":"m","messages":[{"role":"user","content":"\\u0061 literal"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			status, respBody, hits, _ := parseFailE2E(t, "anthropic", nil, body)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", status, respBody)
			}
			if n := atomic.LoadInt32(hits); n != 1 {
				t.Fatalf("upstream calls = %d, want 1", n)
			}
		})
	}
}

// TestParseFailClosedSystemArrayElements: invalid Anthropic system-array
// elements (null, empty object, missing type, non-text type, missing text,
// unrepresentable members) are host 400s with the golden envelope and zero
// upstream; a valid empty-text block is accepted.
func TestParseFailClosedSystemArrayElements(t *testing.T) {
	invalid := []string{
		`{"model":"m","system":[null],"messages":[]}`,
		`{"model":"m","system":[{}],"messages":[]}`,
		`{"model":"m","system":[{"text":"hi"}],"messages":[]}`,
		`{"model":"m","system":[{"type":"tool_use","id":"x","name":"n","input":{}}],"messages":[]}`,
		`{"model":"m","system":[{"type":"text"}],"messages":[]}`,
		`{"model":"m","system":[{"type":"text","text":"hi","citations":[]}],"messages":[]}`,
	}
	for i, body := range invalid {
		t.Run("invalid"+string(rune('a'+i)), func(t *testing.T) {
			status, respBody, hits, _ := parseFailE2E(t, "anthropic", nil, body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", status, respBody)
			}
			if got := string(respBody); got != goldenInvalidRequest("anthropic") {
				t.Fatalf("envelope differs: %s", got)
			}
			if n := atomic.LoadInt32(hits); n != 0 {
				t.Fatalf("upstream called %d times; must be 0", n)
			}
		})
	}
	// Valid empty-text block: accepted, forwarded.
	status, respBody, hits, _ := parseFailE2E(t, "anthropic", nil,
		`{"model":"m","system":[{"type":"text","text":""}],"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("empty-text block status = %d, want 200; body=%s", status, respBody)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}
}

// TestParseFailClosedPassModeTrapper: the non-vacuous hook proof under the
// PASS failure mode. test-trapper traps EVERY before-request invocation; its
// manifest failure mode is block, overridden to pass here. If the local
// parse 400 ever ran a request hook, the trap would fire and (pass mode) the
// request would be forwarded upstream — hits would be 1 and the body would be
// the upstream reply. The golden 400 with zero hits therefore proves the hook
// was skipped, unlike test-observer whose before hook leaves no observable
// trace on a skipped request.
func TestParseFailClosedPassModeTrapper(t *testing.T) {
	status, body, hits, srv := parseFailE2EApproved(t, "anthropic", []string{"test-trapper"}, `not-json{`, http.StatusOK, "pass")
	if status != http.StatusBadRequest || string(body) != goldenInvalidRequest("anthropic") {
		t.Fatalf("pass-mode trapper: status=%d body=%s (a request hook ran and the request leaked upstream)", status, body)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("upstream called %d times; the trap fired, so a request hook RAN on the local 400", n)
	}
	if n := limiterBucketCount(srv); n != 0 {
		t.Fatalf("%d limiter buckets materialized; the fail-closed 400 must not acquire the limiter", n)
	}
}

// TestParseFailClosedBlockModeLimiter: block-mode rows assert zero limiter
// buckets alongside the existing byte-identity proof.
func TestParseFailClosedBlockModeLimiter(t *testing.T) {
	_, _, _, srvBlock := parseFailE2E(t, "anthropic", []string{"test-trapper"}, `not-json{`)
	if n := limiterBucketCount(srvBlock); n != 0 {
		t.Fatalf("block-mode: %d limiter buckets materialized; must be 0", n)
	}
	_, _, _, srvAllow := parseFailE2E(t, "anthropic", []string{"test-observer"}, `not-json{`)
	if n := limiterBucketCount(srvAllow); n != 0 {
		t.Fatalf("allow-mode: %d limiter buckets materialized; must be 0", n)
	}
}
