package wasm

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/economics"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// The dispatcher's refusals are framed classified HostErrors, never v1-style
// status strings smuggled through the value arm. This is the invariant matrix
// for the nine torana_* extension commands: every command × every state it can
// be in — wired (func installed, domain result), nil-func (NOT_CONFIGURED),
// refused (host-callback refusal, passed through classified), invalid (the
// callback returned a malformed ExtensionResult → INTERNAL), malformed args
// (INVALID_ARGUMENT), and ungranted (PERMISSION_DENIED, which the permission
// boundary asserts before the switch).
//
// A refusal row asserts the ERROR arm explicitly, so reverting any case back
// to a `{"status":...}` string body fails that row with "expected a framed
// error, got value" — the revert-proofing property.

type extensionMatrixWant struct {
	arm  string // "value" | "error"
	code pbv2.ErrorCode
	// body is the exact value-arm body for "value" rows.
	body string
	// message is a substring the HostError message must contain for "error" rows.
	message string
}

func (w extensionMatrixWant) check(t *testing.T, name string, res *pbv2.HostCallResult) {
	t.Helper()
	switch w.arm {
	case "value":
		v, ok := res.Result.(*pbv2.HostCallResult_Value)
		if !ok {
			t.Fatalf("%s: expected a value-arm domain body, got error %v", name, res.Result)
		}
		if string(v.Value) != w.body {
			t.Errorf("%s: value body = %q, want %q", name, string(v.Value), w.body)
		}
	case "error":
		e, ok := res.Result.(*pbv2.HostCallResult_Error)
		if !ok {
			t.Fatalf("%s: expected a framed refusal, got value %q", name, res.Result)
		}
		if e.Error.Code != w.code {
			t.Errorf("%s: code = %v, want %v", name, e.Error.Code, w.code)
		}
		if !strings.Contains(e.Error.Message, w.message) {
			t.Errorf("%s: message %q does not contain %q", name, e.Error.Message, w.message)
		}
	}
}

type extensionMatrixRow struct {
	name  string
	cmd   string
	state string // wired | nil-func | refused | invalid | malformed | ungranted
	args  string
	want  extensionMatrixWant
	// invalidResult selects which malformed ExtensionResult an "invalid" row's
	// callback returns: "both" sets value AND refusal, "value-empty" sets a
	// present EMPTY value alongside a refusal, "unspecified" uses an
	// UNSPECIFIED refusal code, "unknown-code" uses a numeric code this build
	// does not recognise.
	invalidResult string
	// refusalCode and refusalMessage parametrize a "refused" row's callback,
	// so a classified refusal can be pinned for every code the seam requires.
	refusalCode    pbv2.ErrorCode
	refusalMessage string
}

// wireExtension installs the host callback a "wired" (or func-backed refusal
// or invalid-result) row needs. The callback reproduces the classification a
// real server would make, so the row proves the dispatcher passes the framed
// error arm through untouched — and that a malformed callback result is
// caught before it can leak to the guest.
func wireExtension(t *testing.T, r *Runtime, row extensionMatrixRow) {
	t.Helper()
	if row.state == "invalid" {
		// The malformed results are constructed DIRECTLY with private fields:
		// the constructors cannot produce them, which is the point — these are
		// host-bug shapes only an in-package test can build.
		switch row.invalidResult {
		case "both":
			bad := ExtensionResult{value: []byte("value"), refusal: hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "refusal")}
			if row.cmd == "verify_virtual_key" {
				r.VerifyVirtualKeyFunc = func(_ context.Context, _ string) ExtensionResult { return bad }
			} else {
				r.SendRequestFunc = func(_ context.Context, _, _ string) ExtensionResult { return bad }
			}
		case "value-empty":
			// A PRESENT empty value alongside a refusal: len(r.value) > 0 used
			// to miss this — the value is empty, but it is still a value, and
			// the guest still cannot tell which arm to trust.
			bad := ExtensionResult{value: []byte{}, refusal: hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "refusal")}
			if row.cmd == "verify_virtual_key" {
				r.VerifyVirtualKeyFunc = func(_ context.Context, _ string) ExtensionResult { return bad }
			} else {
				r.SendRequestFunc = func(_ context.Context, _, _ string) ExtensionResult { return bad }
			}
		case "unspecified":
			bad := ExtensionResult{refusal: hostErr(pbv2.ErrorCode_ERROR_CODE_UNSPECIFIED, "no code")}
			if row.cmd == "verify_virtual_key" {
				r.VerifyVirtualKeyFunc = func(_ context.Context, _ string) ExtensionResult { return bad }
			} else {
				r.SendRequestFunc = func(_ context.Context, _, _ string) ExtensionResult { return bad }
			}
		case "unknown-code":
			// A numeric code this build does not recognise (e.g. 99 from a
			// newer ABI): the guest cannot branch on it, so the dispatcher must
			// frame INTERNAL rather than let it leak to the SDK as a protocol
			// error.
			bad := ExtensionResult{refusal: hostErr(pbv2.ErrorCode(99), "unknown code")}
			if row.cmd == "verify_virtual_key" {
				r.VerifyVirtualKeyFunc = func(_ context.Context, _ string) ExtensionResult { return bad }
			} else {
				r.SendRequestFunc = func(_ context.Context, _, _ string) ExtensionResult { return bad }
			}
		default:
			t.Fatalf("%s: unknown invalid-result variant %q", row.name, row.invalidResult)
		}
		return
	}
	switch row.cmd {
	case "torana_send_request":
		r.SendRequestFunc = func(_ context.Context, _, payload string) ExtensionResult {
			if row.state == "refused" {
				return ExtensionRefusal(pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE, "request to oai failed: boom")
			}
			if !json.Valid([]byte(payload)) {
				return ExtensionRefusal(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid payload")
			}
			return ExtensionValue([]byte(row.want.body))
		}
	case "torana_cache_pricing":
		if row.state == "refused" {
			r.CachePricingFunc = func(_ context.Context, _ string) ExtensionResult {
				return ExtensionRefusal(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "unknown provider")
			}
			return
		}
		r.CachePricingFunc = func(_ context.Context, _ string) ExtensionResult {
			return ExtensionValue([]byte(row.want.body))
		}
	case "torana_record_savings":
		// The canonical callback: the batch-aware report ABI. The legacy
		// two-field SavingsFunc no longer exists.
		r.CompactionReportFunc = func(_ context.Context, _ string, _ economics.CompactionReport) {}
	case "torana_plugin_counter":
		r.PluginCounterFunc = func(string, string, int64) {}
	case "torana_evaluate_compaction":
		if row.state != "wired" {
			// A malformed payload is refused by the dispatcher before the
			// callback is ever consulted.
			return
		}
		var decision economics.CompactionDecision
		if err := json.Unmarshal([]byte(row.want.body), &decision); err != nil {
			t.Fatalf("%s: wired body is not a CompactionDecision: %v", row.name, err)
		}
		r.EvaluateCompactionFunc = func(_ context.Context, _ economics.CompactionReport) economics.CompactionDecision {
			return decision
		}
	case "torana_offload_completion":
		if row.state == "refused" {
			// The canonical OffloadResultFunc is the only callback: the legacy
			// OffloadFunc fallback no longer exists.
			r.OffloadResultFunc = func(_ context.Context, _ string) ExtensionResult {
				// A classified refusal must pass through UNCHANGED: the old
				// dispatcher collapsed every callback error into UNAVAILABLE,
				// which hid caller bugs (INVALID_ARGUMENT) and config gaps
				// (NOT_CONFIGURED) behind a transient-outage story.
				return ExtensionRefusal(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "upstream refused")
			}
			return
		}
		r.OffloadResultFunc = func(_ context.Context, _ string) ExtensionResult {
			return ExtensionValue([]byte(`{"completion":"done"}`))
		}
	case "verify_virtual_key":
		if row.state == "refused" {
			code, message := row.refusalCode, row.refusalMessage
			r.VerifyVirtualKeyFunc = func(_ context.Context, _ string) ExtensionResult {
				return ExtensionRefusal(code, "%s", message)
			}
			return
		}
		r.VerifyVirtualKeyFunc = func(_ context.Context, _ string) ExtensionResult {
			return ExtensionValue([]byte(row.want.body))
		}
	default:
		t.Fatalf("%s: no host callback to wire for %s", row.name, row.cmd)
	}
}

func TestExtensionCommandFramingMatrix(t *testing.T) {
	// A CompactionReport that Normalize()/Valid() accept.
	validReport := `{"original_bytes":1000,"final_bytes":400,"estimated_tokens_removed":100,` +
		`"estimated_rewrite_span_tokens":5000,"expected_applications":1,"source":"transformation"}`

	rows := []extensionMatrixRow{
		// torana_send_request: value arm = provider outcome envelope; refusals
		// are classified by the host callback and pass through framed.
		{name: "send_request/wired", cmd: "torana_send_request", state: "wired",
			args: `{"provider":"oai","request_pb":"e30=","path":"/v1"}`,
			want: extensionMatrixWant{arm: "value", body: `{"http_status":200}`}},
		{name: "send_request/nil-func", cmd: "torana_send_request", state: "nil-func",
			args: `{"provider":"oai","request_pb":"e30=","path":"/v1"}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, message: "plugin egress is not configured"}},
		{name: "send_request/malformed", cmd: "torana_send_request", state: "malformed",
			args: `nonsense`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, message: "invalid payload"}},
		{name: "send_request/refused", cmd: "torana_send_request", state: "refused",
			args: `{"provider":"oai","request_pb":"e30=","path":"/v1"}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE, message: "request to oai failed"}},

		// torana_cache_pricing: data in the value arm — a callback refusal
		// (unknown provider) passes through classified; no callback is
		// NOT_CONFIGURED.
		{name: "cache_pricing/wired", cmd: "torana_cache_pricing", state: "wired",
			args: `{"provider":"oai","model":"gpt-x"}`,
			want: extensionMatrixWant{arm: "value", body: `{"status":"ok","cache_read_usd_per_mtok":1.25}`}},
		{name: "cache_pricing/nil-func", cmd: "torana_cache_pricing", state: "nil-func",
			args: `{"provider":"oai","model":"gpt-x"}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, message: "cache pricing is not configured"}},
		{name: "cache_pricing/refused", cmd: "torana_cache_pricing", state: "refused",
			args: `{"provider":"nope","model":"m"}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, message: "unknown provider"}},

		// torana_db_query / torana_kms_decrypt: no callback exists; the
		// command IS its refusal.
		{name: "db_query/nil-func", cmd: "torana_db_query", state: "nil-func",
			args: `{}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, message: "database not configured"}},
		{name: "kms_decrypt/nil-func", cmd: "torana_kms_decrypt", state: "nil-func",
			args: `{"ciphertext":"AA=="}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, message: "KMS not configured"}},

		// torana_record_savings: success is an EMPTY value arm — the savings
		// were recorded and there is no domain body to acknowledge with.
		{name: "record_savings/wired", cmd: "torana_record_savings", state: "wired",
			args: validReport,
			want: extensionMatrixWant{arm: "value", body: ""}},
		{name: "record_savings/nil-func", cmd: "torana_record_savings", state: "nil-func",
			args: validReport,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, message: "savings tracking not configured"}},
		{name: "record_savings/malformed", cmd: "torana_record_savings", state: "malformed",
			args: `not json`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, message: "invalid payload"}},
		{name: "record_savings/missing-source", cmd: "torana_record_savings", state: "malformed",
			args: `{"original_bytes":1000,"final_bytes":400}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, message: "invalid payload"}},
		{name: "record_savings/removed-legacy-source", cmd: "torana_record_savings", state: "malformed",
			args: `{"original_bytes":1000,"final_bytes":400,"source":"legacy"}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, message: "invalid payload"}},

		// torana_plugin_counter: same split — empty success, refusals framed.
		{name: "plugin_counter/wired", cmd: "torana_plugin_counter", state: "wired",
			args: `{"counter":"c","delta":1}`,
			want: extensionMatrixWant{arm: "value", body: ""}},
		{name: "plugin_counter/nil-func", cmd: "torana_plugin_counter", state: "nil-func",
			args: `{"counter":"c","delta":1}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, message: "plugin counter tracking not configured"}},
		{name: "plugin_counter/malformed", cmd: "torana_plugin_counter", state: "malformed",
			args: `{"counter":""}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, message: "invalid payload"}},

		// torana_evaluate_compaction: real decisions stay domain data; the
		// old {"apply":false,"reason":"pricing_unconfigured"} refusal is now
		// NOT_CONFIGURED.
		{name: "evaluate_compaction/wired", cmd: "torana_evaluate_compaction", state: "wired",
			args: validReport,
			want: extensionMatrixWant{arm: "value", body: `{"apply":true,"reason":"estimated_net_positive"}`}},
		{name: "evaluate_compaction/nil-func", cmd: "torana_evaluate_compaction", state: "nil-func",
			args: validReport,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, message: "compaction pricing is not configured"}},
		{name: "evaluate_compaction/malformed", cmd: "torana_evaluate_compaction", state: "malformed",
			args: `not json`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, message: "invalid payload"}},
		{name: "evaluate_compaction/missing-source", cmd: "torana_evaluate_compaction", state: "malformed",
			args: `{"original_bytes":1000,"final_bytes":400}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, message: "invalid payload"}},
		{name: "evaluate_compaction/removed-legacy-source", cmd: "torana_evaluate_compaction", state: "malformed",
			args: `{"original_bytes":1000,"final_bytes":400,"source":"legacy"}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, message: "invalid payload"}},

		// torana_offload_completion: the callback frames its own success value
		// (no constant status field) and its own classified refusal — the
		// dispatcher passes both through untouched instead of collapsing
		// errors into UNAVAILABLE. Nothing installed is NOT_CONFIGURED.
		{name: "offload_completion/wired", cmd: "torana_offload_completion", state: "wired",
			args: `{}`,
			want: extensionMatrixWant{arm: "value", body: `{"completion":"done"}`}},
		{name: "offload_completion/refused", cmd: "torana_offload_completion", state: "refused",
			args: `{}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, message: "upstream refused"}},
		{name: "offload_completion/nil-func", cmd: "torana_offload_completion", state: "nil-func",
			args: `{}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, message: "offload not configured"}},

		// verify_virtual_key: absent callback is NOT_CONFIGURED — a declared
		// permission that can never succeed in this host is a configuration
		// gap, never UNAVAILABLE (which promises a retryable outage).
		{name: "verify_virtual_key/nil-func", cmd: "verify_virtual_key", state: "nil-func",
			args: `{}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED,
				message: "virtual key verification is not configured"}},
		{name: "verify_virtual_key/wired", cmd: "verify_virtual_key", state: "wired",
			args: `{"key":"vk_abc"}`,
			want: extensionMatrixWant{arm: "value", body: `{"valid":true}`}},
		// Every classified refusal the seam requires passes through a WIRED
		// callback unchanged, code and message exact (the unwired
		// NOT_CONFIGURED row is a different invariant and does not prove a
		// callback's own refusal is preserved).
		{name: "verify_virtual_key/refused-not-configured", cmd: "verify_virtual_key", state: "refused",
			refusalCode: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, refusalMessage: "verifier unwired for tenant",
			args: `{"key":"vk_abc"}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED,
				message: "verifier unwired for tenant"}},
		{name: "verify_virtual_key/refused-unavailable", cmd: "verify_virtual_key", state: "refused",
			refusalCode: pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE, refusalMessage: "verifier backend down",
			args: `{"key":"vk_abc"}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE,
				message: "verifier backend down"}},
		{name: "verify_virtual_key/refused-permission-denied", cmd: "verify_virtual_key", state: "refused",
			refusalCode: pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED, refusalMessage: "verifier refused",
			args: `{"key":"vk_abc"}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED,
				message: "verifier refused"}},
		{name: "verify_virtual_key/refused-invalid-argument", cmd: "verify_virtual_key", state: "refused",
			refusalCode: pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, refusalMessage: "malformed key shape",
			args: `{"key":"vk_abc"}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT,
				message: "malformed key shape"}},
		{name: "verify_virtual_key/invalid-both", cmd: "verify_virtual_key", state: "invalid", invalidResult: "both",
			args: `{"key":"vk_abc"}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INTERNAL,
				message: "extension callback returned an invalid result"}},
		{name: "verify_virtual_key/invalid-value-empty", cmd: "verify_virtual_key", state: "invalid", invalidResult: "value-empty",
			args: `{"key":"vk_abc"}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INTERNAL,
				message: "extension callback returned an invalid result"}},
		{name: "verify_virtual_key/invalid-unspecified", cmd: "verify_virtual_key", state: "invalid", invalidResult: "unspecified",
			args: `{"key":"vk_abc"}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INTERNAL,
				message: "extension callback returned an invalid result"}},
		{name: "verify_virtual_key/invalid-unknown-code", cmd: "verify_virtual_key", state: "invalid", invalidResult: "unknown-code",
			args: `{"key":"vk_abc"}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INTERNAL,
				message: "extension callback returned an invalid result"}},

		// A callback that returns a MALFORMED ExtensionResult must not leak to
		// the guest: both arms set (including a present empty value), an
		// UNSPECIFIED refusal code, or an unknown numeric code is a host-side
		// bug framed as INTERNAL with a loud log. Each shape is constructed
		// directly with private fields — the constructors cannot produce it.
		{name: "send_request/invalid-both", cmd: "torana_send_request", state: "invalid", invalidResult: "both",
			args: `{}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INTERNAL,
				message: "extension callback returned an invalid result"}},
		{name: "send_request/invalid-value-empty", cmd: "torana_send_request", state: "invalid", invalidResult: "value-empty",
			args: `{}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INTERNAL,
				message: "extension callback returned an invalid result"}},
		{name: "send_request/invalid-unspecified", cmd: "torana_send_request", state: "invalid", invalidResult: "unspecified",
			args: `{}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INTERNAL,
				message: "extension callback returned an invalid result"}},
		{name: "send_request/invalid-unknown-code", cmd: "torana_send_request", state: "invalid", invalidResult: "unknown-code",
			args: `{}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INTERNAL,
				message: "extension callback returned an invalid result"}},
	}

	// Ungranted is orthogonal to the command: the permission boundary refuses
	// before the switch, so every command gets the same framed PERMISSION_DENIED.
	for _, cmd := range []string{
		"torana_send_request",
		"torana_cache_pricing",
		"torana_db_query",
		"torana_kms_decrypt",
		"torana_record_savings",
		"torana_plugin_counter",
		"torana_evaluate_compaction",
		"torana_offload_completion",
		"verify_virtual_key",
	} {
		rows = append(rows, extensionMatrixRow{
			name: cmd + "/ungranted", cmd: cmd, state: "ungranted", args: `{}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED,
				message: "permission denied"},
		})
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			var grants []string
			if row.state != "ungranted" {
				grants = []string{"env.host_call." + row.cmd}
			}
			r, p := newGrantedPlugin(t, grants...)

			switch row.state {
			case "wired", "malformed", "refused", "invalid":
				wireExtension(t, r, row)
			case "nil-func", "ungranted":
				// The deliberately-unwired states need no host callback.
			}

			res := hostCallDirect(t, r, p, row.cmd, []byte(row.args))
			row.want.check(t, row.name, res)
		})
	}
}

// TestExtensionResultValidation pins the exclusivity and classification rules
// of the one outcome abstraction every refusing callback returns. These are
// the rules the dispatcher enforces via applyExtensionResult; a callback that
// violates them is a host bug, not a guest input.
//
// The valid list is built ONLY from the constructors and the zero value: a
// constructor-shaped result must always validate. The invalid list is built
// DIRECTLY with the private fields, because the constructors cannot produce
// a malformed result — that is the sum-type discipline F1 restores.
func TestExtensionResultValidation(t *testing.T) {
	valid := []ExtensionResult{
		ExtensionValue([]byte(`{"status":"ok"}`)),
		ExtensionValue(nil), // empty success
		ExtensionValue([]byte{}),
		ExtensionRefusal(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "bad"),
		ExtensionRefusal(pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE, "down"),
		ExtensionResult{}, // zero value: both arms empty — a valid empty success
	}
	for i, r := range valid {
		if err := r.Validate(); err != nil {
			t.Errorf("valid result %d (%+v) rejected: %v", i, r, err)
		}
	}

	invalid := []ExtensionResult{
		// Both arms set: the guest could not tell which one to trust.
		{value: []byte("value"), refusal: hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "refusal")},
		// A PRESENT EMPTY value alongside a refusal: len(r.value) > 0 used to
		// miss this exact shape, letting the malformed envelope reach the SDK.
		{value: []byte{}, refusal: hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "refusal")},
		// A refusal with no classification.
		{refusal: hostErr(pbv2.ErrorCode_ERROR_CODE_UNSPECIFIED, "no code")},
		// A numeric code this build does not recognise: the guest cannot
		// branch on it, so it must not validate.
		{refusal: hostErr(pbv2.ErrorCode(99), "unknown code")},
	}
	for i, r := range invalid {
		if err := r.Validate(); err == nil {
			t.Errorf("invalid result %d (%+v) accepted", i, r)
		}
	}
}

// TestVerifyVirtualKeyPayloadReachesCallbackByteIdentical — the typed
// {"key":...} request body travels from the dispatcher to
// VerifyVirtualKeyFunc byte-identical: a token containing characters that
// need JSON escaping (quotes, backslashes, control characters) arrives
// exactly as the guest wrote it, and the value-arm reply returns unchanged.
func TestVerifyVirtualKeyPayloadReachesCallbackByteIdentical(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.host_call.verify_virtual_key")

	payload := []byte("{\"key\":\"sk-torana-a\\\"b\\\\c\\t\\n\"}")
	var received []byte
	r.VerifyVirtualKeyFunc = func(_ context.Context, args string) ExtensionResult {
		received = []byte(args)
		return ExtensionValue([]byte(`{"status":"ok","tenant_id":"t"}`))
	}

	res := hostCallDirect(t, r, p, "verify_virtual_key", payload)
	if !bytes.Equal(received, payload) {
		t.Fatalf("callback received %q, want the exact payload %q", received, payload)
	}
	if res.GetValue() == nil || !bytes.Equal(res.GetValue(), []byte(`{"status":"ok","tenant_id":"t"}`)) {
		t.Fatalf("value arm did not round-trip unchanged: %v", res)
	}
}
