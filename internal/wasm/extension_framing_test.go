package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/economics"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// The dispatcher's refusals are framed classified HostErrors, never v1-style
// status strings smuggled through the value arm. This is the invariant matrix
// for the nine torana_* extension commands: every command × every state it can
// be in — wired (func installed, domain result), nil-func (NOT_CONFIGURED),
// refused (host-side refusal), unimplemented (verify_virtual_key), malformed
// args (INVALID_ARGUMENT), and ungranted (PERMISSION_DENIED, which the
// permission boundary asserts before the switch).
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
	state string // wired | nil-func | refused | unimplemented | malformed | ungranted
	args  string
	want  extensionMatrixWant
}

// wireExtension installs the host callback a "wired" (or func-backed refusal)
// row needs. The callback reproduces the classification a real server would
// make, so the row proves the dispatcher passes the framed error arm through
// untouched.
func wireExtension(t *testing.T, r *Runtime, row extensionMatrixRow) {
	t.Helper()
	switch row.cmd {
	case "torana_send_request":
		r.SendRequestFunc = func(_ context.Context, _, payload string) (string, *pbv2.HostError) {
			if !json.Valid([]byte(payload)) {
				return "", hostErr(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid payload")
			}
			return row.want.body, nil
		}
	case "torana_cache_pricing":
		r.CachePricingFunc = func(_ context.Context, _ string) string { return row.want.body }
	case "torana_record_savings":
		r.SavingsFunc = func(string, int64, int64) {}
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
		r.OffloadFunc = func(_ context.Context, _ string) (string, error) { return "done", nil }
	default:
		t.Fatalf("%s: no host callback to wire for %s", row.name, row.cmd)
	}
}

func TestExtensionCommandFramingMatrix(t *testing.T) {
	// A CompactionReport that Normalize()/Valid() accept.
	validReport := `{"original_bytes":1000,"final_bytes":400,"estimated_tokens_removed":100,` +
		`"estimated_rewrite_span_tokens":5000,"expected_applications":1}`

	rows := []extensionMatrixRow{
		// torana_send_request: value arm = provider outcome envelope; refusals
		// are classified by the host callback and pass through framed.
		{name: "send_request/wired", cmd: "torana_send_request", state: "wired",
			args: `{"provider":"oai","request_pb":"e30=","path":"/v1"}`,
			want: extensionMatrixWant{arm: "value", body: `{"status":"ok","http_status":200}`}},
		{name: "send_request/nil-func", cmd: "torana_send_request", state: "nil-func",
			args: `{"provider":"oai","request_pb":"e30=","path":"/v1"}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, message: "plugin egress is not configured"}},
		{name: "send_request/malformed", cmd: "torana_send_request", state: "malformed",
			args: `nonsense`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, message: "invalid payload"}},

		// torana_cache_pricing: data in the value arm; no callback is
		// NOT_CONFIGURED.
		{name: "cache_pricing/wired", cmd: "torana_cache_pricing", state: "wired",
			args: `{"provider":"oai","model":"gpt-x"}`,
			want: extensionMatrixWant{arm: "value", body: `{"status":"ok","cache_read_usd_per_mtok":1.25}`}},
		{name: "cache_pricing/nil-func", cmd: "torana_cache_pricing", state: "nil-func",
			args: `{"provider":"oai","model":"gpt-x"}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, message: "cache pricing is not configured"}},

		// torana_db_query / torana_kms_decrypt: no callback exists; the
		// command IS its refusal.
		{name: "db_query/nil-func", cmd: "torana_db_query", state: "nil-func",
			args: `{}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, message: "database not configured"}},
		{name: "kms_decrypt/nil-func", cmd: "torana_kms_decrypt", state: "nil-func",
			args: `{"ciphertext":"AA=="}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, message: "KMS not configured"}},

		// torana_record_savings: {"status":"ok"} is a domain body, kept.
		{name: "record_savings/wired", cmd: "torana_record_savings", state: "wired",
			args: validReport,
			want: extensionMatrixWant{arm: "value", body: `{"status":"ok"}`}},
		{name: "record_savings/nil-func", cmd: "torana_record_savings", state: "nil-func",
			args: validReport,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, message: "savings tracking not configured"}},
		{name: "record_savings/malformed", cmd: "torana_record_savings", state: "malformed",
			args: `not json`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, message: "invalid payload"}},

		// torana_plugin_counter: same split — ok body kept, refusals framed.
		{name: "plugin_counter/wired", cmd: "torana_plugin_counter", state: "wired",
			args: `{"counter":"c","delta":1}`,
			want: extensionMatrixWant{arm: "value", body: `{"status":"ok"}`}},
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

		// torana_offload_completion: success envelope kept; a callback error
		// is UNAVAILABLE with the message preserved; nothing installed is
		// NOT_CONFIGURED.
		{name: "offload_completion/wired", cmd: "torana_offload_completion", state: "wired",
			args: `{}`,
			want: extensionMatrixWant{arm: "value", body: `{"status":"ok","completion":"done"}`}},
		{name: "offload_completion/refused", cmd: "torana_offload_completion", state: "refused",
			args: `{}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE, message: "upstream refused"}},
		{name: "offload_completion/nil-func", cmd: "torana_offload_completion", state: "nil-func",
			args: `{}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, message: "offload not configured"}},

		// verify_virtual_key: no NOT_IMPLEMENTED code exists, so the honest
		// refusal is UNAVAILABLE with the message preserved.
		{name: "verify_virtual_key/unimplemented", cmd: "verify_virtual_key", state: "unimplemented",
			args: `{}`,
			want: extensionMatrixWant{arm: "error", code: pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE,
				message: "unimplemented: enterprise auth is available in torana-edge/private-nucleus"}},
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
			case "wired", "malformed":
				wireExtension(t, r, row)
			case "refused":
				r.OffloadFunc = func(_ context.Context, _ string) (string, error) { return "", errors.New("upstream refused") }
			case "nil-func", "unimplemented", "ungranted":
				// The deliberately-unwired states need no host callback.
			}

			res := hostCallDirect(t, r, p, row.cmd, []byte(row.args))
			row.want.check(t, row.name, res)
		})
	}
}
