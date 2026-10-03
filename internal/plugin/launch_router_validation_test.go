package plugin

import (
	"context"
	"encoding/json"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/pluginstate"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"strings"
	"testing"
)

func TestLaunchAdaptiveRouterOnlyMovesAtNewUserTurn(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "decision_router")
	for _, mode := range []string{"shadow", "auto", "advise", "confirm", "confirm-dismiss"} {
		t.Run(mode, func(t *testing.T) {
			runtime := wasm.NewRuntime(context.Background())
			t.Cleanup(func() { runtime.Close() })
			state, err := pluginstate.New(pluginstate.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { state.Close() })
			runtime.StateGetFunc = state.Get
			runtime.StateSetFunc = state.Set
			runtime.StateGetVersionedFunc = state.GetVersioned
			runtime.StateCompareAndSetFunc = state.CompareAndSet
			runtime.StateScanFunc = state.Scan
			suggestions := 0
			feedback := ""
			runtime.SuggestFunc = func(_ context.Context, name string, args *pbv1.SuggestArgs) (string, *pbv1.HostError) {
				suggestions++
				if name != "decision_router" || args.GetHarnessTargetModel() != "strong-model" {
					t.Fatal("wrong suggestion target")
				}
				if mode == "advise" && len(args.Actions) != 0 {
					t.Fatal("advice offered Torana route acceptance")
				}
				if strings.HasPrefix(mode, "confirm") && len(args.Actions) != 2 {
					t.Fatal("confirmation actions absent")
				}
				return "sg_launch", nil
			}
			runtime.SuggestionOutcomesFunc = func(context.Context, string) ([]byte, error) {
				if feedback == "" {
					return []byte("[]"), nil
				}
				return []byte(`[{"id":"sg_launch","status":"` + feedback + `","via":"ui"}]`), nil
			}
			policy := `{"mode":"MODE","ladders":{"original":{"start":"fast","steps":[{"id":"fast","description":"Routine","model":"fast-model","pricing":{"input":1,"output":5,"cache_read":0.1,"cache_write":1.25}},{"id":"strong","description":"Hard","model":"strong-model","pricing":{"input":5,"output":25,"cache_read":0.5,"cache_write":6.25}}]}},"triggers":{"tool_error_window":3,"tool_error_threshold":1,"reevaluate_every_user_turns":3}}`
			policyMode := mode
			if mode == "confirm-dismiss" {
				policyMode = "confirm"
			}
			policy = strings.Replace(policy, "MODE", policyMode, 1)
			pipeline, err := NewPipeline(runtime, officialPluginConfig(t, bundles, []string{"decision_router"}, map[string]json.RawMessage{"decision_router": json.RawMessage(policy)}))
			if err != nil {
				t.Fatal(err)
			}
			req := &engine.ChatRequest{Model: "fast-model", ToranaMeta: mustOptReqForTest(`{"_conversation_id":"router-session","_provider":"original"}`), Messages: []engine.Message{{Role: engine.RoleSystem, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "coding agent"}}}}, {Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "fix tests"}}}}}}
			if _, err := pipeline.RunBeforeRequest(context.Background(), 1, req, nil); err != nil {
				t.Fatal(err)
			}
			failed := true
			req.Messages = append(req.Messages, engine.Message{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{ID: "failed-call", Name: "shell", Arguments: mustReq("{}")}}}}, engine.Message{Role: engine.RoleTool, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{ToolCallID: "failed-call", ToolName: "shell", IsError: &failed, Content: []engine.ToolResultContentBlock{{Text: "test failed"}}}}}})
			if _, err := pipeline.RunBeforeRequest(context.Background(), 2, req, nil); err != nil {
				t.Fatal(err)
			}
			if route := runtime.VerdictsFor(2).Route(); route != nil {
				t.Fatalf("moved during tool continuation: %+v", route)
			}
			req.Messages = append(req.Messages, engine.Message{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "try again"}}}})
			if _, err := pipeline.RunBeforeRequest(context.Background(), 3, req, nil); err != nil {
				t.Fatal(err)
			}
			route := runtime.VerdictsFor(3).Route()
			if mode == "shadow" && route != nil {
				t.Fatal("shadow routed")
			}
			if mode == "auto" && (route == nil || route.Model != "strong-model" || route.Provider != "original") {
				t.Fatalf("auto did not stage one-step route: %+v", route)
			}
			if mode == "advise" || strings.HasPrefix(mode, "confirm") {
				if route != nil || suggestions != 1 {
					t.Fatalf("suggestion prematurely routed: route=%v suggestions=%d", route, suggestions)
				}
				feedback = "accepted"
				if mode == "confirm-dismiss" {
					feedback = "dismissed"
				}
				if _, err := pipeline.RunBeforeRequest(context.Background(), 4, req, nil); err != nil {
					t.Fatal(err)
				}
				if route := runtime.VerdictsFor(4).Route(); route != nil {
					t.Fatal("confirmation applied on replayed user turn")
				}
				req.Messages = append(req.Messages, engine.Message{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "continue with the fix"}}}})
				if _, err := pipeline.RunBeforeRequest(context.Background(), 5, req, nil); err != nil {
					t.Fatal(err)
				}
				acceptedRoute := runtime.VerdictsFor(5).Route()
				if mode == "confirm" && (acceptedRoute == nil || acceptedRoute.Model != "strong-model") {
					t.Fatalf("accepted next-turn route absent: %v", acceptedRoute)
				}
				if mode != "confirm" && acceptedRoute != nil {
					t.Fatalf("advice or dismissed suggestion routed: %v", acceptedRoute)
				}
			}
		})
	}
}
