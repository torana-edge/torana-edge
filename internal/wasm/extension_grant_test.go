package wasm

import (
	"context"
	"testing"

	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
)

// The host is authoritative for extension-command grants.
//
// The SDK refuses unknown extension tokens guest-side, but that is only an
// ergonomic guard: a handwritten guest never runs the SDK's code. These tests
// exercise the dispatcher directly, which is the real boundary.

// hostCallDirect invokes the dispatcher the way a guest would and decodes the
// framed reply.
func hostCallDirect(t *testing.T, r *Runtime, p *Plugin, cmd string, args []byte) *pbv2.HostCallResult {
	t.Helper()
	raw := r.dispatchHostCallForTest(context.Background(), p.name, cmd, string(args))
	if len(raw) == 0 {
		t.Fatalf("%s returned no reply; HostCallResult requires a result arm", cmd)
	}
	var res pbv2.HostCallResult
	if err := proto.Unmarshal(raw, &res); err != nil {
		t.Fatalf("%s reply does not decode as HostCallResult: %v", cmd, err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("%s reply is not a valid HostCallResult: %v", cmd, err)
	}
	return &res
}

func newGrantedPlugin(t *testing.T, grants ...string) (*Runtime, *Plugin) {
	t.Helper()
	r := NewRuntime(context.Background())
	t.Cleanup(func() { _ = r.Close() })
	p, err := r.LoadPlugin("grant-fixture", MinimalV2Module(false))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p.SetGrants(grants)
	return r, p
}

// An ungranted extension command is refused, and the refusal is FRAMED — a v1
// string envelope would surface inside the guest as a protocol error, so a
// plugin could not tell a missing grant from a broken boundary.
func TestUngrantedExtensionCommandIsFramedPermissionDenied(t *testing.T) {
	r, p := newGrantedPlugin(t) // no grants at all

	res := hostCallDirect(t, r, p, "torana_plugin_counter", []byte(`{"counter":"c","delta":1}`))
	errArm, ok := res.Result.(*pbv2.HostCallResult_Error)
	if !ok {
		t.Fatal("an ungranted extension command succeeded")
	}
	if errArm.Error.Code != pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Fatalf("got %v, want PERMISSION_DENIED", errArm.Error.Code)
	}
}

// The grant is per COMMAND, not a blanket extension capability. Holding one
// extension grant must not open the others.
func TestExtensionGrantIsPerCommand(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.host_call.torana_plugin_counter")

	// The granted one is not refused for permissions. It may still be
	// unconfigured, which is a different code.
	res := hostCallDirect(t, r, p, "torana_plugin_counter", []byte(`{"counter":"c","delta":1}`))
	if e, isErr := res.Result.(*pbv2.HostCallResult_Error); isErr {
		if e.Error.Code == pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
			t.Fatal("a granted command was refused for permissions")
		}
	}

	// A different extension command must still be refused.
	res = hostCallDirect(t, r, p, "torana_offload_completion", []byte(`{}`))
	e, isErr := res.Result.(*pbv2.HostCallResult_Error)
	if !isErr || e.Error.Code != pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Fatal("one extension grant opened a different extension command")
	}
}

// A handwritten guest calling the raw import cannot bypass the grant. The SDK
// check is guest-side and such a guest never runs it, so this is the check that
// actually matters.
func TestHandwrittenGuestCannotBypassTheGrant(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.meta_get") // deliberately unrelated

	for _, cmd := range []string{
		"torana_plugin_counter", // extension
		"env.block_request",     // core verdict
		"env.state_set",         // core store
		pbv2.MetaAppendCommand,  // permission-mapped command
		pbv2.StateDeleteCommand, // permission-mapped command
	} {
		res := hostCallDirect(t, r, p, cmd, nil)
		e, isErr := res.Result.(*pbv2.HostCallResult_Error)
		if !isErr || e.Error.Code != pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
			t.Errorf("%s was not refused for a plugin holding only env.meta_get", cmd)
		}
	}
}

// An unknown command is a framed classified error, not a bare string and not
// silence.
func TestUnknownCommandIsFramed(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.host_call.torana_not_a_command")

	res := hostCallDirect(t, r, p, "torana_not_a_command", nil)
	e, isErr := res.Result.(*pbv2.HostCallResult_Error)
	if !isErr {
		t.Fatal("an unknown command succeeded")
	}
	if e.Error.Code != pbv2.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("got %v, want NOT_FOUND", e.Error.Code)
	}
}

// The two permission-mapped commands must resolve to their namespace's grant,
// not to a capability named after the command — which does not exist, so
// deriving it from the string refuses every call.
func TestPermissionMappedCommandsUseTheirNamespaceGrant(t *testing.T) {
	t.Run("meta_append uses env.meta_set", func(t *testing.T) {
		r, p := newGrantedPlugin(t, pbv2.MetaAppendPermission)
		args, _ := proto.Marshal(&pbv2.MetaAppendArgs{BlockIndex: 0, Fragment: []byte("x")})
		res := hostCallDirect(t, r, p, pbv2.MetaAppendCommand, args)
		if e, isErr := res.Result.(*pbv2.HostCallResult_Error); isErr {
			t.Fatalf("refused despite holding %s: %v", pbv2.MetaAppendPermission, e.Error)
		}
	})
	t.Run("state_delete uses env.state_set", func(t *testing.T) {
		r, p := newGrantedPlugin(t, pbv2.StateDeletePermission)
		r.StateDeleteFunc = func(string, string) error { return nil }
		args, _ := proto.Marshal(&pbv2.StateDeleteArgs{Key: "k"})
		res := hostCallDirect(t, r, p, pbv2.StateDeleteCommand, args)
		if e, isErr := res.Result.(*pbv2.HostCallResult_Error); isErr {
			t.Fatalf("refused despite holding %s: %v", pbv2.StateDeletePermission, e.Error)
		}
	})
}
