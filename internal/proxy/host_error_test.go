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
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

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

// TestHostMarshalFailureNoResponseHook (C1b): with an observer plugin the
// terminal 500 still runs no response hook.
func TestHostMarshalFailureNoResponseHook(t *testing.T) {
	requireWASM(t, "../../examples/plugins/test-invalid-scheduling/plugin.wasm")
	requireWASM(t, "../../examples/plugins/test-observer/plugin.wasm")

	body := `{"model":"m","contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"output":"x"},"id":"c1"}}]}]}`
	_, out, _, srv := parseFailE2E(t, "gemini", []string{"test-invalid-scheduling", "test-observer"}, body)
	if v, ok := srv.sharedCache.Get("observed_error_status"); ok {
		t.Fatalf("a response hook ran after the host_error 500: cache %q", v)
	}
	if string(out) != hostErrorBody("gemini") {
		t.Fatalf("observer changed the terminal body: %s", out)
	}
}

// TestHostErrorGoldenShapes (C2): the exact provider-native value-free 500
// shape per format, pinned INDEPENDENTLY from the renderer.
func TestHostErrorGoldenShapes(t *testing.T) {
	rows := map[string]struct {
		status string // the error.status member for gemini
		code   string // the error.type/code member for the others
	}{
		"anthropic":         {"", "api_error"},
		"openai":            {"", "server_error"},
		"gemini":            {"INTERNAL", ""},
		"gemini-codeassist": {"INTERNAL", ""},
		"bedrock":           {"", "InternalServerException"},
	}
	for format, want := range rows {
		t.Run(format, func(t *testing.T) {
			got := renderHostError(format)
			if got.Status != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", got.Status)
			}
			var m map[string]any
			if err := json.Unmarshal(got.Body, &m); err != nil {
				t.Fatalf("body not JSON: %s", got.Body)
			}
			switch format {
			case "gemini", "gemini-codeassist":
				gerr := m["error"].(map[string]any)
				if gerr["status"] != want.status {
					t.Fatalf("gemini status = %v, want %v", gerr["status"], want.status)
				}
				if gerr["code"] != float64(500) {
					t.Fatalf("gemini code = %v, want 500", gerr["code"])
				}
			case "bedrock":
				if m["message"] == nil {
					t.Fatalf("bedrock envelope missing message: %s", got.Body)
				}
			default:
				gerr := m["error"].(map[string]any)
				if gerr["type"] != want.code {
					t.Fatalf("%s error type = %v, want %v", format, gerr["type"], want.code)
				}
			}
			// Value-free: the body never echoes raw error text.
			if bytes.Contains(got.Body, []byte("scheduling")) {
				t.Fatalf("terminal body leaks a diagnostic: %s", got.Body)
			}
		})
	}
}

// TestBlockPrecedenceBeforeMarshal (C3): a block verdict short-circuits
// BEFORE the marshal seam; the block response wins and no host_error is
// produced even when a later marshal would fail.
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
