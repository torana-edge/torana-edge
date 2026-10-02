package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/torana-edge/torana-edge/internal/cache"
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
			messages := []map[string]string{}
			for _, message := range args.Messages {
				var content strings.Builder
				for _, block := range message.Blocks {
					content.WriteString(block.GetText().GetText())
				}
				messages = append(messages, map[string]string{"role": message.Role, "content": content.String()})
			}
			body, err := json.Marshal(map[string]any{"model": "qwen25-3b", "messages": messages, "max_tokens": args.GetMaxTokens(), "temperature": 0, "response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": args.OutputFormat.Name, "strict": true, "schema": json.RawMessage(args.OutputFormat.SchemaJson)}}})
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
