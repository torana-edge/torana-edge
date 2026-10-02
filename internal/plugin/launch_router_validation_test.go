package plugin

import (
	"context"
	"encoding/json"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/pluginstate"
	"github.com/torana-edge/torana-edge/internal/wasm"
	"strings"
	"testing"
)

func TestLaunchAdaptiveRouterOnlyMovesAtNewUserTurn(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "decision_router")
	for _, mode := range []string{"shadow", "auto"} {
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
			policy := `{"mode":"MODE","ladders":{"original":{"start":"fast","steps":[{"id":"fast","description":"Routine","model":"fast-model","pricing":{"input":1,"output":5,"cache_read":0.1,"cache_write":1.25}},{"id":"strong","description":"Hard","model":"strong-model","pricing":{"input":5,"output":25,"cache_read":0.5,"cache_write":6.25}}]}},"triggers":{"tool_error_window":3,"tool_error_threshold":1,"reevaluate_every_user_turns":3}}`
			policy = strings.Replace(policy, "MODE", mode, 1)
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
		})
	}
}
