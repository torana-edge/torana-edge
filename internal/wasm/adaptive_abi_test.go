package wasm

import (
	"context"
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

func TestModelCapabilitiesHostCallReturnsTypedDeclaration(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.model_capabilities")
	r.ModelCapabilitiesFunc = func(_ context.Context, args *pbv1.ModelCapabilitiesArgs) (*pbv1.ModelCapabilities, *pbv1.HostError) {
		if args.Provider != "p" || args.Model != "m" {
			return nil, &pbv1.HostError{Code: pbv1.ErrorCode_ERROR_CODE_NOT_FOUND, Message: "model is not declared"}
		}
		return &pbv1.ModelCapabilities{Format: "anthropic", EffortLevels: []pbv1.Effort{pbv1.Effort_EFFORT_HIGH}}, nil
	}
	query := func(provider, model string) *pbv1.HostCallResult {
		t.Helper()
		args, err := proto.Marshal(&pbv1.ModelCapabilitiesArgs{Provider: provider, Model: model})
		if err != nil {
			t.Fatal(err)
		}
		return hostCallDirect(t, r, p, "env.model_capabilities", args)
	}
	adaptiveError(t, query("p", "missing"), pbv1.ErrorCode_ERROR_CODE_NOT_FOUND)
	result := query("p", "m")
	value, ok := result.Result.(*pbv1.HostCallResult_Value)
	if !ok {
		t.Fatalf("capability result: %v", result.Result)
	}
	var capabilities pbv1.ModelCapabilities
	if err := proto.Unmarshal(value.Value, &capabilities); err != nil || capabilities.Format != "anthropic" || len(capabilities.EffortLevels) != 1 {
		t.Fatalf("capability body: %+v, %v", &capabilities, err)
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

func TestSuggestHostCallReturnsOnlyHostOwnedID(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.suggest")
	r.SuggestFunc = func(_ context.Context, plugin string, args *pbv1.SuggestArgs) (string, *pbv1.HostError) {
		if plugin != p.name || args.Title != "Try another model?" {
			t.Fatalf("unexpected suggestion call: plugin %q, args %+v", plugin, args)
		}
		return "sg_test", nil
	}
	args, err := proto.Marshal(&pbv1.SuggestArgs{Kind: "model_switch", DedupeKey: "up", Title: "Try another model?", Body: "This might help."})
	if err != nil {
		t.Fatal(err)
	}
	result := hostCallDirect(t, r, p, "env.suggest", args)
	value, ok := result.Result.(*pbv1.HostCallResult_Value)
	if !ok {
		t.Fatalf("suggest result: %v", result.Result)
	}
	var got pbv1.SuggestResult
	if err := proto.Unmarshal(value.Value, &got); err != nil || got.SuggestionId != "sg_test" {
		t.Fatalf("suggest response: %+v, %v", &got, err)
	}
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

func TestEffortRouteRecordsVerdictWhenOperatorEnablesIt(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.route_request", "env.route_request.effort")
	r.RouteEffortEnabledFunc = func(context.Context) bool { return true }
	args, err := proto.Marshal(&pbv1.RouteRequestArgs{Model: "m", Effort: pbv1.Effort_EFFORT_HIGH})
	if err != nil {
		t.Fatal(err)
	}
	result := hostCallDirect(t, r, p, "env.route_request", args)
	if _, refused := result.Result.(*pbv1.HostCallResult_Error); refused {
		t.Fatalf("effort route refused: %v", result.Result)
	}
	if got := r.VerdictsFor(0).Route(); got == nil || got.Effort != pbv1.Effort_EFFORT_HIGH {
		t.Fatalf("effort route verdict = %+v", got)
	}
}
