package wasm

import (
	"context"
	"os"
	"strings"
	"testing"

	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
)

// This file used to pin the v1 denial envelope
// (`{"status":"error","message":"permission denied"}`) as a wire constant that
// published plugin binaries matched verbatim.
//
// That contract is gone. The host refuses any manifest that is not ABI v2, and
// every v2 denial is a framed HostCallResult error arm, so a guest classifies a
// refusal by CODE rather than by matching a string. The tests now pin the
// replacement — including that the old envelope does not come back, since
// reintroducing it would surface inside a v2 guest as a protocol error rather
// than as the refusal it is.

const legacyDenialEnvelope = `{"status":"error","message":"permission denied"}`

// A denial is a framed PERMISSION_DENIED, not a string.
func TestDenialIsFramedNotAStringEnvelope(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()
	p, err := r.LoadPlugin("denial-fixture", MinimalV2Module(false))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p.SetGrants(nil) // no capabilities at all

	raw := r.dispatchHostCallForTest(context.Background(), p.name, "env.meta_get", "")
	if string(raw) == legacyDenialEnvelope {
		t.Fatal("the host returned the v1 denial string; a v2 guest decodes replies as " +
			"HostCallResult, so this surfaces as a protocol error rather than a refusal")
	}
	var res pbv2.HostCallResult
	if err := proto.Unmarshal(raw, &res); err != nil {
		t.Fatalf("denial does not decode as HostCallResult: %v", err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("denial is not a valid HostCallResult: %v", err)
	}
	e, ok := res.Result.(*pbv2.HostCallResult_Error)
	if !ok {
		t.Fatal("a denied call succeeded")
	}
	if e.Error.Code != pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
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
		t.Error("the v1 denial envelope is back in runtime.go. Every v2 denial must be " +
			"a framed HostCallResult error arm; a string envelope is indistinguishable " +
			"from a corrupt reply to a guest that decodes protobuf.")
	}
}

// The unchecked host exports are gone.
//
// env.meta_get read request metadata with NO grant check, so a handwritten
// guest could declare only env.meta_set and still read. env.abort logged
// without env.log. Both bypassed the per-command dispatcher boundary entirely,
// and neither is imported by either v2 SDK.
//
// Asserted against the instantiated host module rather than the source, because
// what matters is what a guest can actually import.
func TestUncheckedHostExportsAreRemoved(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()

	for _, name := range []string{"meta_get", "abort"} {
		if _, err := r.LoadPlugin("importer-"+name, ModuleImportingEnvFunc(name)); err == nil {
			t.Errorf("a guest importing env.%s instantiated. That function has no grant "+
				"check, so a handwritten guest could use it while declaring an unrelated "+
				"capability — bypassing the per-command boundary entirely.", name)
		}
	}

	// Positive control: without it, this test would pass on a host that exports
	// nothing at all.
	//
	// env.host_call takes four i32s, and the fixture declares two, so linking
	// fails either way — but the REASON differs and that is the signal. A
	// signature mismatch means the name resolved, so the function is there; an
	// unknown-import error means it is not.
	_, err := r.LoadPlugin("importer-host_call", ModuleImportingEnvFunc("host_call"))
	if err == nil || !strings.Contains(err.Error(), "signature mismatch") {
		t.Fatalf("env.host_call did not resolve by name (%v); the permission-checked "+
			"path must still exist, or the removals above prove nothing", err)
	}
}
