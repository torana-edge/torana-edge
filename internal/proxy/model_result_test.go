package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// ModelCompleteResult has text and function-call arms. Provider output outside
// that domain must be a classified refusal; otherwise guests cannot tell a
// complete answer from one truncated by the host projection.
func TestModelServiceResultProjectionCoverage(t *testing.T) {
	tests := []struct {
		name, format, path, body string
		wantBlocks               int
		wantText                 string
		wantRefusal              bool
	}{
		{
			name: "chat text and ordinary function", format: "openai", path: "/v1/chat/completions", wantBlocks: 2,
			body: `{"choices":[{"message":{"role":"assistant","content":"answer","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`,
		},
		{
			name: "chat ordinary function only", format: "openai", path: "/v1/chat/completions", wantBlocks: 1,
			body: `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		},
		{
			name: "responses reasoning and final text", format: "openai", path: "/v1/responses", wantBlocks: 1,
			body: `{"status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"private reasoning"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final answer"}]}]}`,
		},
		{
			name: "responses custom freeform tool", format: "openai", path: "/v1/responses", wantRefusal: true,
			body: `{"status":"completed","output":[{"type":"custom_tool_call","call_id":"c1","name":"shell","input":"echo hi"}]}`,
		},
		{
			name: "responses output image", format: "openai", path: "/v1/responses", wantRefusal: true,
			body: `{"status":"completed","output":[{"type":"image_generation_call","id":"img1","result":"base64"}]}`,
		},
		{
			name: "responses refusal part", format: "openai", path: "/v1/responses", wantRefusal: true,
			body: `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"cannot comply"}]}]}`,
		},
		{
			name: "chat malformed function arguments", format: "openai", path: "/v1/chat/completions", wantRefusal: true,
			body: `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"not-json"}}]},"finish_reason":"tool_calls"}]}`,
		},
		{
			name: "anthropic thinking and final text", format: "anthropic", path: "/v1/messages", wantBlocks: 1,
			body: `{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"private"},{"type":"text","text":"final answer"}],"stop_reason":"end_turn"}`,
		},
		{
			name: "anthropic server tool output", format: "anthropic", path: "/v1/messages", wantRefusal: true,
			body: `{"type":"message","role":"assistant","content":[{"type":"server_tool_use","id":"c1","name":"web_search","input":{"q":"x"}}],"stop_reason":"tool_use"}`,
		},
		{
			name: "gemini inline image", format: "gemini", path: "/v1beta/models/m:generateContent", wantRefusal: true,
			body: `{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]},"finishReason":"STOP"}]}`,
		},
		{
			name: "gemini text with image sibling", format: "gemini", path: "/v1beta/models/m:generateContent", wantRefusal: true,
			body: `{"candidates":[{"content":{"role":"model","parts":[{"text":"partial","inlineData":{"mimeType":"image/png","data":"AA=="}}]},"finishReason":"STOP"}]}`,
		},
		{
			name: "gemini thought and final text", format: "gemini", path: "/v1beta/models/m:generateContent", wantBlocks: 1, wantText: "final answer",
			body: `{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"private reasoning"},{"text":"final answer"}]},"finishReason":"STOP"}]}`,
		},
		{
			name: "gemini reasoning only", format: "gemini", path: "/v1beta/models/m:generateContent", wantRefusal: true,
			body: `{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"private reasoning"}]},"finishReason":"STOP"}]}`,
		},
		{
			name: "empty typed message", format: "openai", path: "/v1/chat/completions", wantRefusal: true,
			body: `{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"stop"}]}`,
		},
		{
			name: "duplicate nested function name", format: "openai", path: "/v1/chat/completions", wantRefusal: true,
			body: `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"first","name":"second","arguments":"{}"}}]}}]}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()
			providers := testProviderConfig(upstream.URL, "bound", tc.format)
			providers.Providers["bound"] = provider.Provider{URL: upstream.URL, Format: tc.format, Auth: provider.ProviderAuth{Mode: "none"}}
			server, err := New(Config{Port: "0", Providers: providers})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Shutdown(context.Background())
			result, refusal := server.completeModel(context.Background(), "fixture", wasm.ModelServiceResource{
				Name: "service", Provider: "bound", Model: "m", Path: tc.path, Timeout: time.Second,
				MaxTokens: 40, MaxInputBytes: 2000, MaxCallsPerMinute: 10, MaxTokensPerHour: 1000,
			}, &pbv1.ModelCompleteArgs{Service: "service", Messages: []*pbv1.Message{{Role: "user", Blocks: modelTextBlocks("question")}}})
			if tc.wantRefusal {
				if result != nil || refusal == nil || refusal.Code != pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
					t.Fatalf("unsupported result was not an UNAVAILABLE refusal: result=%v refusal=%v", result, refusal)
				}
				return
			}
			if refusal != nil || result == nil || result.Message == nil || len(result.Message.Blocks) != tc.wantBlocks {
				t.Fatalf("representable result failed: result=%v refusal=%v", result, refusal)
			}
			if tc.wantText != "" && result.Message.Blocks[0].GetText().GetText() != tc.wantText {
				t.Fatalf("projected text = %q, want %q", result.Message.Blocks[0].GetText().GetText(), tc.wantText)
			}
		})
	}
}
