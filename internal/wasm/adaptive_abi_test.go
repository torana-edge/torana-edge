package wasm

import (
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
)

func adaptiveError(t *testing.T, result *pbv1.HostCallResult, want pbv1.ErrorCode) {
	t.Helper()
	arm, ok := result.Result.(*pbv1.HostCallResult_Error)
	if !ok || arm.Error.Code != want {
		t.Fatalf("host call result = %v, want %v", result.Result, want)
	}
}

func TestAdaptiveHostCallsRefuseUntilConfigured(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.suggest", "env.model_capabilities")
	suggest, err := proto.Marshal(&pbv1.SuggestArgs{
		Kind: "model_switch", DedupeKey: "upgrade", Title: "Use another model?", Body: "This task may benefit from it.",
	})
	if err != nil {
		t.Fatal(err)
	}
	adaptiveError(t, hostCallDirect(t, r, p, "env.suggest", suggest), pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED)
	adaptiveError(t, hostCallDirect(t, r, p, "env.suggest", nil), pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT)

	capabilities, err := proto.Marshal(&pbv1.ModelCapabilitiesArgs{Provider: "p", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	adaptiveError(t, hostCallDirect(t, r, p, "env.model_capabilities", capabilities), pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED)
	adaptiveError(t, hostCallDirect(t, r, p, "env.model_capabilities", nil), pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT)
}

func TestEffortRouteRequiresSeparateGrantAndHostSupport(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.route_request")
	args, err := proto.Marshal(&pbv1.RouteRequestArgs{Model: "m", Effort: pbv1.Effort_EFFORT_HIGH})
	if err != nil {
		t.Fatal(err)
	}
	adaptiveError(t, hostCallDirect(t, r, p, "env.route_request", args), pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED)
	p.SetGrants([]string{"env.route_request", "env.route_request.effort"})
	adaptiveError(t, hostCallDirect(t, r, p, "env.route_request", args), pbv1.ErrorCode_ERROR_CODE_UNSUPPORTED)

	plain, err := proto.Marshal(&pbv1.RouteRequestArgs{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	result := hostCallDirect(t, r, p, "env.route_request", plain)
	if _, refused := result.Result.(*pbv1.HostCallResult_Error); refused {
		t.Fatalf("unspecified effort changed existing route behavior: %v", result.Result)
	}
}
