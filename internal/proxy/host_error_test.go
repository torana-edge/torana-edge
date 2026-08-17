package proxy

// PR B proofs: the terminal host_error 500 for a HOST MARSHAL FAILURE
// after a contract-valid accepted replacement (MARSHAL_FAILURE_CHECKPOINT
// §5 + the authorization matrix):
//
//   - C1: real E2E — the invalid-scheduling plugin produces an
//     SDK-valid replacement the gemini adapter cannot marshal; the
//     response is the exact provider-native value-free 500 with zero
//     upstream, zero limiter buckets, no response hooks / upstream
//     status, no compaction credit, and the feed verdict host_error with
//     PluginFailure=false.
//   - C2: the per-format golden 500 shapes (renderHostError unit rows).
//   - C3: block/respond precedence — a block verdict short-circuits
//     BEFORE marshal and never enters the host_error path.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/economics"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
)

// hostErrorBody is the gemini provider-native 500 envelope.
func hostErrorBody(format string) string {
	// Reuse the production renderer as the golden source is circular; the
	// SHAPES are pinned independently below for every format.
	return string(renderHostError(format).Body)
}

// TestHostMarshalFailureTerminalE2E (C1): the real transport serves the
// host_error 500 when a contract-valid replacement cannot be marshaled.
func TestHostMarshalFailureTerminalE2E(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-invalid-scheduling/plugin.wasm")

	body := `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1"}}]}]}`
	status, out, hits, srv := parseFailE2E(t, "gemini", []string{"test-invalid-scheduling"}, body)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", status, out)
	}
	// The EXACT provider-native value-free body (gemini INTERNAL shape).
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("500 body is not the gemini error envelope: %s", out)
	}
	gerr, ok := got["error"].(map[string]any)
	if !ok || gerr["status"] != "INTERNAL" || gerr["code"] != float64(500) {
		t.Fatalf("500 body not the gemini INTERNAL envelope: %s", out)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("upstream called %d times; the terminal 500 must never reach upstream", n)
	}
	if n := limiterBucketCount(srv); n != 0 {
		t.Fatalf("%d limiter buckets materialized; the terminal 500 must not acquire the limiter", n)
	}
	// No response hook ran (the observer would record observed_error_status)
	// and no upstream status was recorded; the feed verdict is host_error
	// with PluginFailure=false.
	events := srv.feed.Snapshot()
	if len(events) == 0 {
		t.Fatal("no feed event recorded")
	}
	last := events[len(events)-1]
	if last.Verdict != "host_error" {
		t.Fatalf("feed verdict = %q, want host_error", last.Verdict)
	}
	if last.PluginFailure {
		t.Fatal("a host marshal failure is NOT a plugin failure (PluginFailure must be false)")
	}
	// No provider-reported outcome on the terminal path: the caller sees
	// the 500, and no tokens/cache counters can be attributed to an
	// upstream that was never called.
	if last.Status != http.StatusInternalServerError {
		t.Fatalf("feed status = %d, want 500", last.Status)
	}
	if last.TokensIn != 0 || last.TokensOut != 0 || last.CacheReadTokens != 0 || last.CacheWriteTokens != 0 {
		t.Fatalf("provider-reported counters on the terminal path: %+v", last)
	}
	// No compaction credit on the terminal path: the prepared report is
	// discarded, so no compaction application can be recorded.
	if got := srv.stats.Snapshot().CompactionApplications; got != 0 {
		t.Fatalf("compaction applications = %d; the terminal path must grant no compaction credit", got)
	}
}

// TestHostErrorSanitizedLog (C1s) — the secret-bearing pin: the invalid
// scheduling value carries a unique guest-controlled secret. The response
// is the value-free literal, and the COMPLETE captured log must omit the
// secret AND the raw adapter diagnostic (the adapter error embeds the
// scheduling string — interpolating it would leak request data a plugin
// could load with secrets). Exactly ONE structurally matched host-error
// line may exist.
func TestHostErrorSanitizedLog(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-invalid-scheduling/plugin.wasm")

	body := `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1"}}]}]}`
	sink := captureLogs(t)
	status, out, hits, _ := parseFailE2E(t, "gemini", []string{"test-invalid-scheduling"}, body)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", status, out)
	}
	if !bytes.Equal(out, literalHostError("gemini")) {
		t.Fatalf("body = %s, want the value-free literal (no diagnostic echo)", out)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("upstream calls = %d, want 0", n)
	}
	logs := sink.String()
	const secret = "SECRET-7f3d9c2a-SCHEDULING"
	if strings.Contains(logs, secret) {
		t.Fatalf("guest-controlled secret leaked into the log: %q", logs)
	}
	if strings.Contains(logs, "tool result scheduling") {
		t.Fatalf("raw adapter diagnostic leaked into the log: %q", logs)
	}
	// EXACTLY ONE structurally matched sanitized line: a timestamp-aware,
	// anchored expression for the full line — any extra text, a different
	// format name, or a duplicated/truncated line fails.
	lineRe := regexp.MustCompile(`(?m)^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} format gemini: host marshal failure — serving host_error$`)
	if m := lineRe.FindAllString(logs, -1); len(m) != 1 {
		t.Fatalf("sanitized host-error lines = %d, want exactly 1; log: %q", len(m), logs)
	}
}

// TestHostMarshalFailureNoResponseHook (C1b): with an observer plugin the
// terminal 500 still runs no response hook.
func TestHostMarshalFailureNoResponseHook(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-invalid-scheduling/plugin.wasm")
	requireWASM(t, "../../examples/plugins/test-observer/plugin.wasm")

	body := `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1"}}]}]}`
	_, out, _, srv := parseFailE2E(t, "gemini", []string{"test-invalid-scheduling", "test-observer"}, body)
	if v, ok := srv.sharedCache.Get(observerCacheKey("observed_error_status")); ok {
		t.Fatalf("a response hook ran after the host_error 500: cache %q", v)
	}
	if string(out) != hostErrorBody("gemini") {
		t.Fatalf("observer changed the terminal body: %s", out)
	}
}

// TestHostErrorGoldenShapes (C2): the exact provider-native value-free 500
// shape per format, pinned INDEPENDENTLY from the renderer.
func TestHostErrorGoldenShapes(t *testing.T) {
	// INDEPENDENT literal golden bytes per configured format: the exact
	// provider-native value-free 500. Byte equality rejects any added,
	// missing, or reordered member — map-shaped assertions would permit
	// drift.
	literal := map[string]struct {
		body []byte
		ct   string
	}{
		"anthropic": {
			[]byte(`{"error":{"message":"the request could not be encoded for the provider","type":"api_error"},"type":"error"}`),
			"application/json",
		},
		"openai": {
			[]byte(`{"error":{"code":"server_error","message":"the request could not be encoded for the provider","type":"server_error"}}`),
			"application/json",
		},
		"gemini": {
			[]byte(`{"error":{"code":500,"message":"the request could not be encoded for the provider","status":"INTERNAL"}}`),
			"application/json",
		},
		"gemini-codeassist": {
			[]byte(`{"error":{"code":500,"message":"the request could not be encoded for the provider","status":"INTERNAL"}}`),
			"application/json",
		},
		"bedrock": {
			[]byte(`{"message":"the request could not be encoded for the provider"}`),
			"application/json",
		},
	}
	for format, want := range literal {
		t.Run(format+"/unit", func(t *testing.T) {
			got := renderHostError(format)
			if got.Status != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", got.Status)
			}
			if got.ContentType != want.ct {
				t.Fatalf("content-type = %q, want %q", got.ContentType, want.ct)
			}
			if !bytes.Equal(got.Body, want.body) {
				t.Fatalf("body\n got: %s\nwant: %s", got.Body, want.body)
			}
		})
	}
}

// TestHostErrorPerFormatTerminal (C2) drives EVERY configured format
// through the ACTUAL server terminal: a contract-valid replacement the
// format's adapter cannot marshal (a real provider-specific reproduction,
// not a registry mutation) must produce the exact literal 500, zero
// upstream, and the host_error verdict. The gemini row is the real-WASM
// reachability row.
func TestHostErrorPerFormatTerminal(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-invalid-scheduling/plugin.wasm")
	requireWASM(t, "../../examples/plugins/test-tool-role-message/plugin.wasm")
	requireWASM(t, "../../examples/plugins/test-redacted-thinking/plugin.wasm")

	geminiBody := `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1"}}]}]}`
	codeAssistBody := `{"model":"m","request":{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1"}}]}]}}`

	rows := []struct {
		format string
		plugin string
		body   string
	}{
		{"gemini", "test-invalid-scheduling", geminiBody},
		{"gemini-codeassist", "test-invalid-scheduling", codeAssistBody},
		{"anthropic", "test-tool-role-message", `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`},
		{"openai", "test-redacted-thinking", `{"model":"m","messages":[{"role":"user","content":"redactme"}]}`},
		{"bedrock", "test-redacted-thinking", `{"modelId":"m","messages":[{"role":"user","content":[{"text":"redactme"}]}]}`},
	}
	for _, row := range rows {
		t.Run(row.format, func(t *testing.T) {
			status, body, hdr, hits, srv := parseFailE2EWithHeader(t, row.format, []string{row.plugin}, row.body, http.StatusOK, "")
			if ct := hdr.Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json through HTTP", ct)
			}
			if status != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body=%s", status, body)
			}
			want := literalHostError(row.format)
			if !bytes.Equal(body, want) {
				t.Fatalf("body\n got: %s\nwant: %s", body, want)
			}
			if n := atomic.LoadInt32(hits); n != 0 {
				t.Fatalf("upstream called %d times; must be 0", n)
			}
			events := srv.feed.Snapshot()
			if len(events) == 0 {
				t.Fatal("no feed event")
			}
			last := events[len(events)-1]
			if last.Verdict != "host_error" || last.PluginFailure {
				t.Fatalf("verdict = %q plugin_failure=%v, want host_error/false", last.Verdict, last.PluginFailure)
			}
		})
	}
}

// literalHostError is the independent golden bytes (single source for the
// terminal rows; kept separate from the renderer so drift is caught).
func literalHostError(format string) []byte {
	switch format {
	case "anthropic":
		return []byte(`{"error":{"message":"the request could not be encoded for the provider","type":"api_error"},"type":"error"}`)
	case "openai":
		return []byte(`{"error":{"code":"server_error","message":"the request could not be encoded for the provider","type":"server_error"}}`)
	case "gemini", "gemini-codeassist":
		return []byte(`{"error":{"code":500,"message":"the request could not be encoded for the provider","status":"INTERNAL"}}`)
	default: // bedrock
		return []byte(`{"message":"the request could not be encoded for the provider"}`)
	}
}

func TestBlockPrecedenceBeforeMarshal(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-blocker/plugin.wasm")
	requireWASM(t, "../../examples/plugins/test-invalid-scheduling/plugin.wasm")

	// The blocker triggers on the marker text; the invalid-scheduling
	// plugin would fail marshal AFTER it — precedence says the block wins
	// and the marshal seam is never reached.
	body := `{"model":"m","contents":[{"role":"user","parts":[{"text":"blockme"}]},{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1"}}]}]}`
	status, out, hits, _ := parseFailE2E(t, "gemini", []string{"test-blocker", "test-invalid-scheduling"}, body)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("block precedence: status = %d, want 422 (the block response); body=%s", status, out)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("upstream calls = %d, want 0 (block short-circuits before marshal and upstream)", n)
	}
	if bytes.Contains(out, []byte("could not be encoded")) {
		t.Fatalf("host_error body leaked despite the block verdict: %s", out)
	}
}

// TestHostErrorStateModel (D) — the reference state model distinguishing
// the three outcomes after plugin observation:
//
//   - plugin-output-refused (pass): the invalid replacement is dropped and
//     the request continues with the ACCEPTED input (upstream 1, 200);
//   - plugin-output-refused (block): the plugin refusal (502, verdict
//     block, zero upstream) — LAYER A failure_mode semantics;
//   - valid-output-host-marshal-failed: the SDK-valid replacement cannot
//     be projected onto the wire — host_error 500, verdict host_error,
//     PluginFailure FALSE, zero upstream — LAYER B terminal, independent
//     of plugin failure mode;
//   - block/respond short-circuit BEFORE marshal and never enter the
//     host_error path (precedence).
func TestHostErrorStateModel(t *testing.T) {
	body := `{"model":"m","request":{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1"}}]}]}}`
	blockBody := `{"model":"m","request":{"contents":[{"role":"user","parts":[{"text":"blockme"}]},{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1"}}]}]}}`

	requireWASM(t, "../../examples/plugins/test-invalid-scheduling/plugin.wasm")
	requireWASM(t, "../../examples/plugins/test-envelope-smuggler/plugin.wasm")
	requireWASM(t, "../../examples/plugins/test-blocker/plugin.wasm")

	smugDigest := mustDigest(t, "test-envelope-smuggler")
	smugPerms := manifestPermissions("../../examples/plugins/test-envelope-smuggler")

	rows := []struct {
		name              string
		order             []string
		approvals         map[string]provider.PluginApproval
		body              string
		wantStatus        int
		wantHits          int32
		wantVerdict       string
		wantPluginFailure bool
	}{
		{
			// Layer A pass: the smuggled (provider-invalid) replacement is
			// rolled back; the accepted input continues upstream.
			name:      "plugin-output-refused pass continues accepted input",
			order:     []string{"test-envelope-smuggler"},
			approvals: map[string]provider.PluginApproval{"test-envelope-smuggler": {Digest: smugDigest, Permissions: smugPerms, FailureMode: "pass"}},
			body:      body, wantStatus: http.StatusOK, wantHits: 1, wantVerdict: "",
		},
		{
			// Layer A block: the plugin refusal, verdict block, zero upstream.
			name:      "plugin-output-refused block refuses",
			order:     []string{"test-envelope-smuggler"},
			approvals: map[string]provider.PluginApproval{"test-envelope-smuggler": {Digest: smugDigest, Permissions: smugPerms, FailureMode: "block"}},
			body:      body, wantStatus: 502, wantHits: 0, wantVerdict: "block",
		},
		{
			// Layer B: SDK-valid replacement, host marshal failure — the
			// terminal host_error, NOT a plugin failure.
			name:  "valid-output host-marshal-failed",
			order: []string{"test-invalid-scheduling"}, approvals: map[string]provider.PluginApproval{},
			body: body, wantStatus: http.StatusInternalServerError, wantHits: 0,
			wantVerdict: "host_error",
		},
		{
			// Precedence: block short-circuits before the marshal seam.
			name:  "block precedes host_error",
			order: []string{"test-blocker", "test-invalid-scheduling"}, approvals: map[string]provider.PluginApproval{},
			body: blockBody, wantStatus: http.StatusUnprocessableEntity, wantHits: 0, wantVerdict: "block",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			status, out, hits, srv := parseFailE2EWithValidator(t, "gemini", row.order, row.body, row.approvals)
			if status != row.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", status, row.wantStatus, out)
			}
			if n := atomic.LoadInt32(hits); n != row.wantHits {
				t.Fatalf("upstream calls = %d, want %d", n, row.wantHits)
			}
			events := srv.feed.Snapshot()
			if len(events) == 0 {
				t.Fatal("no feed event")
			}
			last := events[len(events)-1]
			if last.Verdict != row.wantVerdict {
				t.Fatalf("verdict = %q, want %q", last.Verdict, row.wantVerdict)
			}
			if last.PluginFailure != row.wantPluginFailure {
				t.Fatalf("plugin_failure = %v, want %v", last.PluginFailure, row.wantPluginFailure)
			}
		})
	}
}

func mustDigest(t *testing.T, name string) string {
	t.Helper()
	d, err := plugin.BundleDigestForDir("../../examples/plugins/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestApplyHostErrorClearsQueuedReport — unit revert-proof for the
// terminal: seeded with a realistic PRIOR route state (Verdict="route",
// non-empty VerdictPlugin, queued attributed compaction report, prepared
// flag), applyHostError must atomically produce host_error: clear the
// route verdict/attribution, clear PluginFailure, install the 500 block,
// and clear the report state. Reverting ANY owned assignment (verdict,
// attribution, synthetic, block, discard) fails this test; the production
// path owns the transition exactly once (no caller-side duplicate).
func TestApplyHostErrorClearsQueuedReport(t *testing.T) {
	rs := &reqState{
		Verdict:                   "route",
		VerdictPlugin:             "test-router",
		Synthetic:                 false,
		PluginFailure:             true,
		CompactionReports:         []attributedCompactionReport{{Plugin: "p", Report: economics.CompactionReport{OriginalBytes: 100, FinalBytes: 40}}},
		CompactionRequestPrepared: true,
	}
	rc := &RouteContext{}
	prov := &provider.Provider{Format: "gemini"}
	applyHostError(rs, rc, prov)

	if rc.Block == nil || rc.Block.Status != http.StatusInternalServerError {
		t.Fatalf("block = %+v, want the 500 host_error", rc.Block)
	}
	if !bytes.Equal(rc.Block.Body, literalHostError("gemini")) {
		t.Fatalf("block body = %s, want the literal gemini host_error", rc.Block.Body)
	}
	if rc.Block.ContentType != "application/json" {
		t.Fatalf("block content-type = %q, want application/json", rc.Block.ContentType)
	}
	if rs.Verdict != "host_error" {
		t.Fatalf("verdict = %q, want host_error (the route verdict must be replaced)", rs.Verdict)
	}
	if rs.VerdictPlugin != "" {
		t.Fatalf("verdict plugin = %q, want empty", rs.VerdictPlugin)
	}
	if rs.PluginFailure {
		t.Fatal("plugin_failure must be false on the terminal")
	}
	if !rs.Synthetic {
		t.Fatal("synthetic must be true on the terminal")
	}
	if len(rs.CompactionReports) != 0 {
		t.Fatalf("queued report survived the terminal: %+v", rs.CompactionReports)
	}
	if rs.CompactionRequestPrepared || rs.CompactionReportsCommitted {
		t.Fatalf("compaction flags survived the terminal: prepared=%v committed=%v", rs.CompactionRequestPrepared, rs.CompactionReportsCommitted)
	}
}

// TestHostErrorDiscardsQueuedReportNonVacuous — the compaction-credit
// proof with a REAL guest-queued report:
//
//  1. CONTROL: the extension-contract fixture queues a valid attributed
//     report (record-savings ack landed in the upstream body) and the
//     request completes — the server records ONE compaction application,
//     proving the harness counter moves.
//  2. TERMINAL: the same report is queued, THEN the invalid-scheduling
//     fixture poisons a tool result — the host_error terminal fires and
//     the queued report is discarded: the application counter stays at 1.
//
// If the terminal committed the queued report, the counter would be 2.
func TestHostErrorDiscardsQueuedReportNonVacuous(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-extension-contract/plugin.wasm")
	requireWASM(t, "../../examples/plugins/test-redacted-thinking/plugin.wasm")

	var upstreamBodies []string
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		b, _ := io.ReadAll(r.Body)
		upstreamBodies = append(upstreamBodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(upstream.Close)

	provCfg := provider.Config{
		Providers: map[string]provider.Provider{"p": {URL: upstream.URL, Format: "openai"}},
		Limits:    provider.Limits{Concurrency: 8},
		Plugins:   provider.PluginsConfig{Dir: "../../examples/plugins", Order: []string{"test-extension-contract", "test-redacted-thinking"}, AllowUnapproved: true},
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
	post := func(model string, poison bool) (int, []byte) {
		content := `{"role":"user","content":"hi"}`
		if poison {
			content = `{"role":"user","content":"redactme"}`
		}
		body := `{"model":"` + model + `","messages":[` + content + `]}`
		req, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/provider/p"+inferenceTestPath("openai"), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	// CONTROL: the report is queued (the guest ack lands in the replaced
	// model on the wire) and the request completes — one application.
	status, body := post("record-savings", false)
	if status != http.StatusOK {
		t.Fatalf("control status = %d; body=%s", status, body)
	}
	if len(upstreamBodies) != 1 || !strings.Contains(upstreamBodies[0], `raw_succeeded\":true`) {
		// The observation rides inside the replaced model string, so the
		// wire shows the JSON escaped (\"raw_succeeded\":true).
		t.Fatalf("control did not reach upstream with the record-savings ack: %q", upstreamBodies)
	}
	if got := srv.stats.Snapshot().CompactionApplications; got != 1 {
		t.Fatalf("control recorded %d applications, want 1 (the counter must move)", got)
	}

	// TERMINAL: same queue, then the marshal failure (the redacted-thinking
	// fixture appends a redacted block the openai adapter refuses) — the
	// report must be discarded, never committed.
	status, body = post("record-savings", true)
	if status != http.StatusInternalServerError {
		t.Fatalf("terminal status = %d, want 500; body=%s", status, body)
	}
	if !bytes.Equal(body, literalHostError("openai")) {
		t.Fatalf("terminal body = %s, want the literal openai host_error", body)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("upstream calls = %d, want exactly the control's 1", got)
	}
	if got := srv.stats.Snapshot().CompactionApplications; got != 1 {
		t.Fatalf("terminal recorded %d applications, want 1 — the queued report must be discarded, not committed", got)
	}
}

// TestHostErrorFailureModeIndependence — the SAME valid-output
// marshal-failure guest under pass AND block approvals: the host_error
// terminal is independent of plugin failure mode (the replacement was
// contract-valid, so failure_mode never applies), and the outcomes are
// byte-identical.
func TestHostErrorFailureModeIndependence(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-invalid-scheduling/plugin.wasm")
	body := `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1"}}]}]}`
	var want []byte
	for _, fm := range []string{"pass", "block"} {
		t.Run(fm, func(t *testing.T) {
			status, out, hdr, hits, srv := parseFailE2EApprovedH(t, "gemini", []string{"test-invalid-scheduling"}, body, http.StatusOK, fm)
			if ct := hdr.Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json through HTTP", ct)
			}
			if status != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body=%s", status, out)
			}
			if want == nil {
				want = out
			} else if !bytes.Equal(out, want) {
				t.Fatalf("pass/block outcomes differ:\n pass: %s\nblock: %s", want, out)
			}
			if n := atomic.LoadInt32(hits); n != 0 {
				t.Fatalf("upstream calls = %d, want 0", n)
			}
			events := srv.feed.Snapshot()
			last := events[len(events)-1]
			if last.Verdict != "host_error" || last.PluginFailure {
				t.Fatalf("verdict = %q plugin_failure=%v, want host_error/false", last.Verdict, last.PluginFailure)
			}
		})
	}
	if !bytes.Equal(want, literalHostError("gemini")) {
		t.Fatalf("host_error body = %s, want the literal gemini shape", want)
	}
}

// TestRespondPrecedesHostError — a responder verdict wins over a would-be
// marshal failure: the exact respond body/status is served, no host_error
// log line or body appears, and upstream is never called.
func TestRespondPrecedesHostError(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-responder/plugin.wasm")
	requireWASM(t, "../../examples/plugins/test-invalid-scheduling/plugin.wasm")

	body := `{"model":"m","contents":[{"role":"user","parts":[{"text":"respondme please"}]},{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1"}}]}]}`
	sink := captureLogs(t)
	status, out, hdr, hits, srv := parseFailE2EWithHeader(t, "gemini", []string{"test-responder", "test-invalid-scheduling"}, body, http.StatusOK, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want the responder's 200; body=%s", status, out)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json through HTTP", ct)
	}
	// EXACT independent bytes of the gemini direct-completion envelope —
	// extra members, a host-error envelope with the canned text, or
	// provider-envelope drift all fail.
	want := []byte(`{"candidates":[{"content":{"parts":[{"text":"canned response from test-responder"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":0,"promptTokenCount":0,"totalTokenCount":0}}`)
	if !bytes.Equal(out, want) {
		t.Fatalf("respond body\n got: %s\nwant: %s", out, want)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("upstream calls = %d, want 0", n)
	}
	if strings.Contains(sink.String(), "host marshal failure") {
		t.Fatalf("host_error log line appeared on the respond path: %q", sink.String())
	}
	events := srv.feed.Snapshot()
	last := events[len(events)-1]
	if last.Verdict != "respond" {
		t.Fatalf("feed verdict = %q, want respond", last.Verdict)
	}
}

// TestHostErrorReplacesRouteVerdict — a model-only route verdict was
// applied BEFORE the marshal seam; the terminal must replace the feed
// verdict with host_error while retaining the routed model/provider as
// diagnostics. Neither original nor routed upstream is called.
func TestHostErrorReplacesRouteVerdict(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-router/plugin.wasm")
	requireWASM(t, "../../examples/plugins/test-invalid-scheduling/plugin.wasm")

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(upstream.Close)

	provCfg := provider.Config{
		Providers: map[string]provider.Provider{"main": {URL: upstream.URL, Format: "gemini"}},
		Limits:    provider.Limits{Concurrency: 8},
		Plugins:   provider.PluginsConfig{Dir: "../../examples/plugins", Order: []string{"test-router", "test-invalid-scheduling"}, AllowUnapproved: true},
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

	body := `{"model":"m","contents":[{"role":"user","parts":[{"text":"routemodel"}]},{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1"}}]}]}`
	client := &http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/provider/main"+inferenceTestPath("gemini"), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", resp.StatusCode, out)
	}
	if !bytes.Equal(out, literalHostError("gemini")) {
		t.Fatalf("body = %s, want the literal gemini host_error", out)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("upstream calls = %d, want 0", n)
	}
	events := srv.feed.Snapshot()
	last := events[len(events)-1]
	if last.Verdict != "host_error" {
		t.Fatalf("feed verdict = %q, want host_error (the route verdict must be replaced)", last.Verdict)
	}
	if last.PluginFailure {
		t.Fatalf("plugin failure = %v, want false", last.PluginFailure)
	}
	// The routed model is retained as a diagnostic fact.
	if last.Model != "tiny-model" {
		t.Fatalf("feed model = %q, want the routed tiny-model", last.Model)
	}
}
