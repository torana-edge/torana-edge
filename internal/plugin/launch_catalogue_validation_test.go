package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/torana-edge/torana-edge/internal/cache"
	"github.com/torana-edge/torana-edge/internal/economics"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/pluginstate"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func launchSharedIntentKey(conversation, id, name, args string) string {
	if args == "" {
		args = "{}"
	}
	var values map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &values) != nil || values == nil {
		return ""
	}
	delete(values, "i")
	normalized, _ := json.Marshal(values)
	h := sha256.New()
	for _, s := range []string{conversation, id, name, string(normalized)} {
		h.Write([]byte(strconv.Itoa(len(s)) + ":" + s))
	}
	return "intent/shared/v3:sha256:" + hex.EncodeToString(h.Sum(nil))
}

func TestLaunchConstrainedSchemaSubtreesStayExact(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "schema_translator")
	pipeline := newTestPipeline(t, bundles, []string{"schema_translator"})
	for index, raw := range []string{
		`{"type":"object","additionalProperties":false,"properties":{"env":{"type":"object","enum":[{"prod":"on"}],"additionalProperties":true}}}`,
		`{"type":"object","additionalProperties":false,"properties":{"rows":{"type":"array","enum":[[{"env":{"prod":"on"}}]],"items":{"type":"object","properties":{"env":{"type":"object","additionalProperties":true}}}}}}`,
	} {
		request := &engine.ChatRequest{Tools: []engine.ToolDef{{Name: "run", Parameters: mustReq(raw)}}}
		out, err := pipeline.RunBeforeRequest(context.Background(), uint64(index+1), request, nil)
		if err != nil {
			t.Fatal(err)
		}
		if out == nil {
			out = request
		}
		if string(out.Tools[0].Parameters.Bytes()) != raw {
			t.Fatalf("constrained schema was weakened: %s", out.Tools[0].Parameters.Bytes())
		}
	}
}

func TestLaunchCompactorModelCacheReusesExactReplacement(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "compactor")
	pipeline := newTestPipelineWith(t, bundles, []string{"compactor"}, cache.NewLocalCache(time.Minute), map[string]json.RawMessage{"compactor": json.RawMessage(`{"tool_policies":[{"match":"read","mode":"model"}],"expected_applications":6}`)})
	calls, reports := 0, 0
	pipeline.runtime.ModelCompleteFunc = func(_ context.Context, name string, resource wasm.ModelServiceResource, args *pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError) {
		calls++
		if name != "compactor" || args.Service != "summarizer" {
			t.Fatal("wrong model binding")
		}
		return &pbv1.ModelCompleteResult{Message: &pbv1.ResponseMessage{Blocks: []*pbv1.ResponseBlock{{Kind: &pbv1.ResponseBlock_Text{Text: &pbv1.ResponseTextBlock{Text: "Relevant evidence in server.go."}}}}}, Usage: &pbv1.Usage{InputTokens: 100, OutputTokens: 8}}, nil
	}
	pipeline.runtime.EvaluateCompactionFunc = func(_ context.Context, report economics.CompactionReport, target wasm.PricingResource, summarizer *wasm.PricingResource) economics.CompactionDecision {
		return economics.CompactionDecision{Apply: true}
	}
	pipeline.runtime.CompactionReportFunc = func(_ context.Context, name string, report economics.CompactionReport, target wasm.PricingResource, summarizer *wasm.PricingResource) {
		reports++
	}
	for turn := uint64(1); turn <= 3; turn++ {
		request := cacheComplianceRequest()
		request.Model = "target"
		request.ToranaMeta = mustOptReqForTest(`{"_conversation_id":"model-compaction","_provider":"test"}`)
		request.Messages = append(request.Messages, engine.Message{Role: engine.RoleAssistant, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "I read the complete result."}}}})
		out, err := pipeline.RunBeforeRequest(context.Background(), turn, request, nil)
		if err != nil {
			t.Fatal(err)
		}
		if out == nil {
			out = request
		}
		if got := toolResultText(out.Messages[3]); got != "Relevant evidence in server.go." {
			t.Fatalf("summary missing: %q", got)
		}
	}
	if calls != 1 || reports != 3 {
		t.Fatalf("model replacement not replay-stable: model=%d reports=%d", calls, reports)
	}
}
func TestLaunchCatalogueAgentOperations(t *testing.T) {
	bundles := officialBundlesDir(t)
	for _, name := range []string{"cache_tier_selector", "cache_warmer", "compactor", "decision_router", "intent", "keyword_compactor", "otel", "pii", "pii_guard", "tool_governor", "usage_logger"} {
		t.Run(name, func(t *testing.T) {
			requireBundle(t, bundles, name)
			raw, err := os.ReadFile(bundles + "/" + name + "/agent.json")
			if err != nil {
				t.Fatal(err)
			}
			var descriptor AgentDescriptor
			if err := json.Unmarshal(raw, &descriptor); err != nil {
				t.Fatal(err)
			}
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
			runtime.StateCompareAndDeleteFunc = state.CompareAndDelete
			runtime.StateKeysFunc = state.Keys
			runtime.StateScanFunc = state.Scan
			conversation := "catalogue-session"
			runtime.ExecutionInfoFunc = func(context.Context) *pbv1.ExecutionInfo { return &pbv1.ExecutionInfo{ConversationId: &conversation} }
			cfg := map[string]json.RawMessage{}
			if name == "tool_governor" {
				cfg[name] = json.RawMessage(`{"allow":["read"]}`)
			}
			pipeline, err := NewPipeline(runtime, officialPluginConfig(t, bundles, []string{name}, cfg))
			if err != nil {
				t.Fatal(err)
			}
			for index, operation := range descriptor.Operations {
				if _, _, found := pipeline.FindAgentOperation(name, operation.Method, operation.Path); !found {
					t.Fatalf("operation missing from discovery: %s", operation.ID)
				}
				ctx, err := WithMCPBinding(context.Background(), MCPBinding{Bound: true, ConversationID: conversation, CallID: "catalogue-confirmed-call"})
				if err != nil {
					t.Fatal(err)
				}
				request := &pbv1.HttpRequest{Method: operation.Method, Path: "/agent" + operation.Path}
				if operation.Method == "POST" {
					request.Body = []byte(`{"tool":"shell"}`)
				}
				if operation.ConversationBinding == "required" {
					unbound, err := pipeline.RunOnHTTPRequest(context.Background(), uint64(index+100), name, request, nil)
					if err != nil || unbound == nil || unbound.Status != 409 {
						t.Fatalf("%s allowed unbound scope: response=%v err=%v", operation.ID, unbound, err)
					}
				}
				response, err := pipeline.RunOnHTTPRequest(ctx, uint64(index+1), name, request, nil)
				if err != nil || response == nil || response.Status != 200 {
					t.Fatalf("%s failed: response=%v err=%v", operation.ID, response, err)
				}
				if err := ValidateConfigAgainstSchema(&ConfigSchema{Raw: operation.OutputSchema}, response.Body); err != nil {
					t.Fatalf("%s violates its advertised output: %v; body=%s", operation.ID, err, response.Body)
				}
				t.Logf("compiled agent operation %s:%s PASS", name, operation.ID)
			}
		})
	}
}

func TestLaunchIntentSharedOccurrenceIsolation(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "intent")
	store := cache.NewLocalCache(time.Minute)
	pp := newTestPipelineWith(t, bundles, []string{"intent"}, store, map[string]json.RawMessage{"intent": json.RawMessage(`{"fill":"off"}`)})
	for index, row := range []struct{ conversation, path, intent string }{
		{"A", "server.go", "inspect authentication"},
		{"B", "server.go", "inspect retry"},
		{"A", "another.go", "inspect cache"},
	} {
		reqID := uint64(index + 1)
		registerIntentConversation(t, pp, reqID, row.conversation)
		runAs(t, pp, reqID, toolStart(0, "reused-id", "read"))
		args, _ := json.Marshal(map[string]string{"path": row.path, "i": row.intent})
		runAs(t, pp, reqID, toolDelta(0, string(args)))
		runAs(t, pp, reqID, toolEnd(0))
	}
	for _, row := range []struct{ conversation, path, intent string }{
		{"A", "server.go", "inspect authentication"},
		{"B", "server.go", "inspect retry"},
		{"A", "another.go", "inspect cache"},
	} {
		args, _ := json.Marshal(map[string]string{"path": row.path})
		got, found, err := store.Get(context.Background(), wasm.SharedCacheKey(launchSharedIntentKey(row.conversation, "reused-id", "read", string(args))))
		if err != nil || !found || got != row.intent {
			t.Fatalf("cross-task handoff corrupt: %s/%s got %q found=%v err=%v", row.conversation, row.path, got, found, err)
		}
	}
	if _, found, err := store.Get(context.Background(), wasm.SharedCacheKey("intent:reused-id")); err != nil || found {
		t.Fatal("global call-ID intent published")
	}
}

func TestLaunchPIIModelVerdictReplay(t *testing.T) {
	bundles := officialBundlesDir(t)
	requireBundle(t, bundles, "pii")
	runtime := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { runtime.Close() })
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	conversation := "launch-pii"
	runtime.ExecutionInfoFunc = func(context.Context) *pbv1.ExecutionInfo { return &pbv1.ExecutionInfo{ConversationId: &conversation} }
	runtime.StateGetFunc = state.Get
	runtime.StateSetFunc = state.Set
	runtime.StateGetVersionedFunc = state.GetVersioned
	runtime.StateCompareAndSetFunc = state.CompareAndSet
	runtime.StateCompareAndDeleteFunc = state.CompareAndDelete
	pp, err := NewPipeline(runtime, officialPluginConfig(t, bundles, []string{"pii"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	pp.runtime.ModelCompleteFunc = func(_ context.Context, plugin string, service wasm.ModelServiceResource, args *pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError) {
		calls++
		if plugin != "pii" || args.Service != "scanner" || len(args.Messages) != 2 {
			t.Fatalf("unexpected scanner invocation: %s/%s", plugin, args.Service)
		}
		var text strings.Builder
		for _, block := range args.Messages[1].Blocks {
			text.WriteString(block.GetText().GetText())
		}
		if !strings.Contains(text.String(), "DB_PASSWORD=synthetic-password") {
			t.Fatal("scanner did not receive tool output")
		}
		if endpoint := os.Getenv("TORANA_LOCAL_SCANNER_URL"); endpoint != "" {
			model := os.Getenv("TORANA_LOCAL_SCANNER_MODEL")
			if model == "" {
				t.Fatal("TORANA_LOCAL_SCANNER_MODEL is required")
			}
			messages := []map[string]string{}
			for _, message := range args.Messages {
				var content strings.Builder
				for _, block := range message.Blocks {
					content.WriteString(block.GetText().GetText())
				}
				messages = append(messages, map[string]string{"role": message.Role, "content": content.String()})
			}
			body, err := json.Marshal(map[string]any{"model": model, "messages": messages, "max_tokens": args.GetMaxTokens(), "temperature": 0, "response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": args.OutputFormat.Name, "strict": true, "schema": json.RawMessage(args.OutputFormat.SchemaJson)}}})
			if err != nil {
				t.Fatal(err)
			}
			client := http.Client{Timeout: 90 * time.Second}
			response, err := client.Post(endpoint+"/v1/chat/completions", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			if err != nil || response.StatusCode != 200 {
				t.Fatalf("local scanner failed: status=%d err=%v", response.StatusCode, err)
			}
			var reply struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(raw, &reply); err != nil || len(reply.Choices) != 1 {
				t.Fatalf("local scanner malformed: %v", err)
			}
			t.Logf("local scanner returned %s", reply.Choices[0].Message.Content)
			return &pbv1.ModelCompleteResult{FinishReason: reply.Choices[0].FinishReason, Message: &pbv1.ResponseMessage{Blocks: []*pbv1.ResponseBlock{{Kind: &pbv1.ResponseBlock_Text{Text: &pbv1.ResponseTextBlock{Text: reply.Choices[0].Message.Content}}}}}}, nil
		}
		return &pbv1.ModelCompleteResult{Message: &pbv1.ResponseMessage{Blocks: []*pbv1.ResponseBlock{{Kind: &pbv1.ResponseBlock_Text{Text: &pbv1.ResponseTextBlock{Text: `{"pii":true,"findings":[{"type":"password","line":1}]}`}}}}}}, nil
	}
	request := func() *engine.ChatRequest {
		return &engine.ChatRequest{ToranaMeta: mustOptReqForTest(`{"_conversation_id":"launch-pii"}`), Tools: []engine.ToolDef{{Name: "read", Parameters: mustReq(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
			Messages: []engine.Message{
				{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "inspect configuration"}}}},
				{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{ID: "call-1", Name: "read", Arguments: mustReq(`{"path":"config.txt"}`)}}}},
				{Role: engine.RoleTool, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{ToolCallID: "call-1", ToolName: "read", Content: []engine.ToolResultContentBlock{{Text: "DB_PASSWORD=synthetic-password"}}}}}},
			}}
	}
	for turn := uint64(1); turn <= 3; turn++ {
		req := request()
		if turn > 1 {
			req.Messages = append(req.Messages, engine.Message{Role: engine.RoleUser, Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "continue"}}}})
		}
		out, err := pp.RunBeforeRequest(context.Background(), turn, req, nil)
		if err != nil {
			t.Fatal(err)
		}
		if out == nil {
			out = req
		}
		text := toolResultText(out.Messages[2])
		if strings.Contains(text, "synthetic-password") || !strings.Contains(text, "line 1") {
			t.Fatalf("unsafe or unrecoverable replacement: %q", text)
		}
	}
	if calls != 1 {
		t.Fatalf("historical occurrence rescanned %d times", calls)
	}
}
