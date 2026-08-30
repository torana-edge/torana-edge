package wasm

import (
	"context"
	"os"
	"strings"
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
)

// This file pins the typed denial envelope
// (`{"status":"error","message":"permission denied"}`) as a wire constant that
// published plugin binaries matched verbatim.
//
// That contract is gone. The host refuses any manifest that is not ABI v1, and
// every current ABI denial is a framed HostCallResult error arm, so a guest classifies a
// refusal by CODE rather than by matching a string. The tests now pin the
// replacement — including that the old envelope does not come back, since
// reintroducing it would surface inside a plugin guest as a protocol error rather
// than as the refusal it is.

const legacyDenialEnvelope = `{"status":"error","message":"permission denied"}`

// A denial is a framed PERMISSION_DENIED, not a string.
func TestDenialIsFramedNotAStringEnvelope(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()
	p, err := r.LoadPlugin("denial-fixture", MinimalModule(false))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p.SetGrants(nil) // no capabilities at all

	raw := r.dispatchHostCallForTest(context.Background(), p.name, "env.meta_get", "")
	if string(raw) == legacyDenialEnvelope {
		t.Fatal("the host returned the legacy denial string; a plugin guest decodes replies as " +
			"HostCallResult, so this surfaces as a protocol error rather than a refusal")
	}
	var res pbv1.HostCallResult
	if err := proto.Unmarshal(raw, &res); err != nil {
		t.Fatalf("denial does not decode as HostCallResult: %v", err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("denial is not a valid HostCallResult: %v", err)
	}
	e, ok := res.Result.(*pbv1.HostCallResult_Error)
	if !ok {
		t.Fatal("a denied call succeeded")
	}
	if e.Error.Code != pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Fatalf("got %v, want PERMISSION_DENIED", e.Error.Code)
	}
}

// The legacy envelope must not reappear anywhere in the runtime.
func TestLegacyDenialEnvelopeIsGone(t *testing.T) {
	src, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), legacyDenialEnvelope) {
		t.Error("the legacy denial envelope is back in runtime.go. Every denial must be " +
			"a framed HostCallResult error arm; a string envelope is indistinguishable " +
			"from a corrupt reply to a guest that decodes protobuf.")
	}
}

// The unchecked host exports are gone.
//
// env.meta_get read request metadata with NO grant check, so a handwritten
// guest could declare only env.meta_set and still read. env.abort logged
// without env.log. Both bypassed the per-command dispatcher boundary entirely,
// and neither is imported by either current ABI SDK.
//
// Asserted against the instantiated host module rather than the source, because
// what matters is what a guest can actually import.
func TestUncheckedHostExportsAreRemoved(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()

	for _, name := range []string{"meta_get", "abort"} {
		_, err := r.LoadPlugin("importer-"+name, ModuleImportingEnvFunc(name))
		if err == nil {
			t.Errorf("a guest importing env.%s instantiated. That function has no grant "+
				"check, so a handwritten guest could use it while declaring an unrelated "+
				"capability — bypassing the per-command boundary entirely.", name)
			continue
		}
		// The REASON matters. The fixture now declares each import's real
		// signature, so a restored side door would link — or fail on something
		// other than being absent. Accepting any error meant a restored
		// env.abort (four i32s, no result) failed on a signature mismatch and
		// the test still passed.
		if strings.Contains(err.Error(), "signature mismatch") {
			t.Errorf("env.%s failed to link on a SIGNATURE MISMATCH, which means the "+
				"function still exists: %v", name, err)
		}
	}

	// Positive control: without it, this test would pass on a host that exports
	// nothing at all. The fixture declares host_call's real signature, so a
	// guest importing it must LINK.
	if _, err := r.LoadPlugin("importer-host_call", ModuleImportingEnvFunc("host_call")); err != nil {
		t.Fatalf("a guest importing env.host_call failed to link: %v — the "+
			"permission-checked path must still exist, or the removals above "+
			"prove nothing", err)
	}
}

// Originals: absence and a captured empty value must be different answers.
//
// The callbacks are installed unconditionally, so "returned nil" cannot mean
// unavailable — on the streaming and upstream-error paths nothing is ever
// snapshotted. An all-default ChatRequest marshals to ZERO BYTES and an
// upstream body can legitimately be empty, so length is not presence.
func TestOriginalsDistinguishAbsenceFromCapturedEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
		set  func(r *Runtime, captured bool, payload []byte)
	}{
		{"request", "env.original_request", func(r *Runtime, captured bool, payload []byte) {
			r.OriginalRequestFunc = func(context.Context) ([]byte, bool) { return payload, captured }
		}},
		{"response", "env.original_response", func(r *Runtime, captured bool, payload []byte) {
			r.OriginalResponseFunc = func(context.Context) ([]byte, bool) { return payload, captured }
		}},
	} {
		t.Run(tc.name+"/absent is NOT_FOUND", func(t *testing.T) {
			r, p := newGrantedPlugin(t, tc.cmd)
			tc.set(r, false, nil)
			res := hostCallDirect(t, r, p, tc.cmd, nil)
			e, isErr := res.Result.(*pbv1.HostCallResult_Error)
			if !isErr {
				t.Fatal("an uncaptured original reported success")
			}
			if e.Error.Code != pbv1.ErrorCode_ERROR_CODE_NOT_FOUND {
				t.Fatalf("got %v, want NOT_FOUND", e.Error.Code)
			}
		})

		t.Run(tc.name+"/captured empty is a successful empty value", func(t *testing.T) {
			r, p := newGrantedPlugin(t, tc.cmd)
			tc.set(r, true, nil)
			res := hostCallDirect(t, r, p, tc.cmd, nil)
			v, isVal := res.Result.(*pbv1.HostCallResult_Value)
			if !isVal {
				t.Fatalf("a captured empty original was reported as an error: %+v", res.Result)
			}
			if len(v.Value) != 0 {
				t.Fatalf("value = %q, want empty", v.Value)
			}
		})

		t.Run(tc.name+"/captured non-empty round trips", func(t *testing.T) {
			r, p := newGrantedPlugin(t, tc.cmd)
			tc.set(r, true, []byte("pristine"))
			res := hostCallDirect(t, r, p, tc.cmd, nil)
			v, isVal := res.Result.(*pbv1.HostCallResult_Value)
			if !isVal || string(v.Value) != "pristine" {
				t.Fatalf("got %+v, want the captured bytes", res.Result)
			}
		})
	}
}
