package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/bridge"
	"github.com/torana-edge/torana-edge/internal/engine"
)

// bridgeUpstreamSSE is deliberately literal provider wire. These fixtures do
// not pass through Torana's serializers before reaching the proxy under test.
func bridgeUpstreamSSE(p bridge.Protocol, tool bool) string {
	switch p {
	case bridge.OpenAIChat:
		if tool {
			return strings.Join([]string{
				`data: {"id":"reply_stream","model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_123","type":"function","function":{"name":"weather"}}]}}]}`,
				`data: {"id":"reply_stream","model":"upstream-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"n\":"}}]}}]}`,
				`data: {"id":"reply_stream","model":"upstream-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"9007199254740993}"}}]},"finish_reason":"tool_calls"}]}`,
				`data: {"id":"reply_stream","model":"upstream-model","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13,"prompt_tokens_details":{"cached_tokens":3}}}`,
				`data: [DONE]`,
			}, "\n\n")
		}
		return strings.Join([]string{
			`data: {"id":"reply_stream","model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant","content":"It is "}}]}`,
			`data: {"id":"reply_stream","model":"upstream-model","choices":[{"index":0,"delta":{"content":"sunny"},"finish_reason":"stop"}]}`,
			`data: {"id":"reply_stream","model":"upstream-model","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13,"prompt_tokens_details":{"cached_tokens":3}}}`,
			`data: [DONE]`,
		}, "\n\n")
	case bridge.OpenAIResponses:
		if tool {
			return strings.ReplaceAll(strings.Join([]string{
				`event: response.created\ndata: {"type":"response.created","response":{"id":"reply_stream","model":"upstream-model","status":"in_progress"}}`,
				`event: response.output_item.added\ndata: {"type":"response.output_item.added","output_index":0,"item":{"id":"item_123","type":"function_call","call_id":"call_123","name":"weather"}}`,
				`event: response.function_call_arguments.delta\ndata: {"type":"response.function_call_arguments.delta","item_id":"item_123","output_index":0,"delta":"{\"n\":"}`,
				`event: response.function_call_arguments.delta\ndata: {"type":"response.function_call_arguments.delta","item_id":"item_123","output_index":0,"delta":"9007199254740993}"}`,
				`event: response.function_call_arguments.done\ndata: {"type":"response.function_call_arguments.done","item_id":"item_123","output_index":0,"arguments":"{\"n\":9007199254740993}"}`,
				`event: response.output_item.done\ndata: {"type":"response.output_item.done","output_index":0,"item":{"id":"item_123","type":"function_call","call_id":"call_123","name":"weather","arguments":"{\"n\":9007199254740993}"}}`,
				`event: response.completed\ndata: {"type":"response.completed","response":{"id":"reply_stream","model":"upstream-model","status":"completed","output":[{"id":"item_123","type":"function_call","call_id":"call_123","name":"weather","arguments":"{\"n\":9007199254740993}"}],"usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`,
			}, "\n\n"), `\n`, "\n")
		}
		return strings.ReplaceAll(strings.Join([]string{
			`event: response.created\ndata: {"type":"response.created","response":{"id":"reply_stream","model":"upstream-model","status":"in_progress"}}`,
			`event: response.output_item.added\ndata: {"type":"response.output_item.added","output_index":0,"item":{"id":"message_123","type":"message","role":"assistant","content":[]}}`,
			`event: response.output_text.delta\ndata: {"type":"response.output_text.delta","item_id":"message_123","output_index":0,"content_index":0,"delta":"It is sunny"}`,
			`event: response.completed\ndata: {"type":"response.completed","response":{"id":"reply_stream","model":"upstream-model","status":"completed","output":[{"id":"message_123","type":"message","role":"assistant","content":[{"type":"output_text","text":"It is sunny","annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`,
		}, "\n\n"), `\n`, "\n")
	case bridge.Anthropic:
		start := `data: {"type":"message_start","message":{"id":"reply_stream","role":"assistant","model":"upstream-model","usage":{"input_tokens":7,"cache_read_input_tokens":3}}}`
		if tool {
			return strings.Join([]string{
				start,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_123","name":"weather","input":{}}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"n\":"}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"9007199254740993}"}}`,
				`data: {"type":"content_block_stop","index":0}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`,
				`data: {"type":"message_stop"}`,
			}, "\n\n")
		}
		return strings.Join([]string{
			start,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"It is sunny"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			`data: {"type":"message_stop"}`,
		}, "\n\n")
	case bridge.Gemini, bridge.GeminiCodeAssist:
		part := `{"functionCall":{"id":"call_123","name":"weather","args":{"n":9007199254740993}}}`
		if !tool {
			part = `{"text":"It is sunny"}`
		}
		chunk := fmt.Sprintf(`{"responseId":"reply_stream","modelVersion":"upstream-model","candidates":[{"index":0,"content":{"role":"model","parts":[%s]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":13,"cachedContentTokenCount":3}}`, part)
		if p == bridge.GeminiCodeAssist {
			chunk = `{"response":` + chunk + `}`
		}
		return "data: " + chunk
	default:
		panic("unknown bridge protocol")
	}
}

func bridgeStreamingClientBody(p bridge.Protocol, second bool) string {
	body := bridgeClientBody(p, second, true)
	if p == bridge.OpenAIChat {
		body = strings.Replace(body, `"stream":true`, `"stream":true,"stream_options":{"include_usage":true}`, 1)
	}
	return body
}

func TestBridgeStreamingToolLoopAllCrossProtocolPairs(t *testing.T) {
	for _, clientProtocol := range bridgeProtocols {
		for _, upstreamProtocol := range bridgeProtocols {
			if clientProtocol == upstreamProtocol {
				continue
			}
			t.Run(string(clientProtocol)+"_to_"+string(upstreamProtocol), func(t *testing.T) {
				var calls atomic.Int32
				proxy, _ := newBridgeProxy(t, clientProtocol, upstreamProtocol, func(w http.ResponseWriter, r *http.Request) {
					turn := calls.Add(1)
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					chat, err := bridge.ParseRequest(upstreamProtocol, raw, r.URL.Path)
					if err != nil {
						t.Errorf("translated upstream request: %v; %s", err, raw)
						return
					}
					if !chat.Stream {
						t.Errorf("upstream request lost stream=true: %s", raw)
					}
					if turn == 2 {
						var results int
						for _, message := range chat.Messages {
							for _, block := range message.Blocks {
								if block.ToolResult != nil && block.ToolResult.ToolCallID == "call_123" {
									results++
								}
							}
						}
						if results != 1 {
							t.Errorf("translated tool loop lost result identity: %s", raw)
						}
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, bridgeUpstreamSSE(upstreamProtocol, turn == 1))
				})

				for turn := 1; turn <= 2; turn++ {
					status, raw, headers := callBridge(t, proxy, clientProtocol, bridgeStreamingClientBody(clientProtocol, turn == 2), true)
					if status != http.StatusOK || !strings.HasPrefix(headers.Get("Content-Type"), "text/event-stream") {
						t.Fatalf("turn %d status=%d content-type=%q body=%s", turn, status, headers.Get("Content-Type"), raw)
					}
					assertBridgeStreamWireTerminal(t, clientProtocol, raw)
					events := collectBridgeStreamEvents(bridge.ParseStream(clientProtocol, bytes.NewReader(raw)))
					assertBridgeClientEvents(t, events, turn == 1, clientProtocol)
				}
				if calls.Load() != 2 {
					t.Fatalf("upstream calls=%d", calls.Load())
				}
			})
		}
	}
}

func collectBridgeStreamEvents(events <-chan engine.StreamEvent) []engine.StreamEvent {
	var all []engine.StreamEvent
	for event := range events {
		all = append(all, event)
	}
	return all
}

func assertBridgeClientEvents(t *testing.T, events []engine.StreamEvent, wantTool bool, destination bridge.Protocol) {
	t.Helper()
	var id, text, callID, args, finish string
	var usage *engine.StreamUsage
	for _, event := range events {
		switch {
		case event.Error != nil:
			t.Fatalf("client stream parse failed: %s; events=%+v", event.Error.Message, events)
		case event.MessageStart != nil:
			id = event.MessageStart.ID
		case event.TextDelta != nil:
			text += *event.TextDelta
		case event.ToolCallStart != nil:
			callID = event.ToolCallStart.ID
		case event.ToolCallDelta != nil:
			args += event.ToolCallDelta.ArgumentsDelta
		case event.Usage != nil:
			usage = event.Usage
		case event.FinishReason != "":
			finish = event.FinishReason
		}
	}
	if id != "reply_stream" || usage == nil || usage.OutputTokens != 3 {
		t.Fatalf("identity/usage lost: id=%q usage=%+v events=%+v", id, usage, events)
	}
	wantInput := 10
	if destination == bridge.Anthropic {
		wantInput = 7
	}
	if usage.InputTokens != wantInput {
		t.Fatalf("destination usage input=%d, want %d; %+v", usage.InputTokens, wantInput, usage)
	}
	if wantTool {
		if callID != "call_123" || args != `{"n":9007199254740993}` || finish != "tool_calls" {
			t.Fatalf("tool stream call=%q args=%q finish=%q events=%+v", callID, args, finish, events)
		}
	} else if text != "It is sunny" || finish != "stop" {
		t.Fatalf("text stream text=%q finish=%q events=%+v", text, finish, events)
	}
}

func assertBridgeStreamWireTerminal(t *testing.T, p bridge.Protocol, raw []byte) {
	t.Helper()
	marker := map[bridge.Protocol]string{
		bridge.OpenAIChat:       "data: [DONE]",
		bridge.OpenAIResponses:  `"type":"response.completed"`,
		bridge.Anthropic:        `"type":"message_stop"`,
		bridge.Gemini:           `"finishReason":"STOP"`,
		bridge.GeminiCodeAssist: `"finishReason":"STOP"`,
	}[p]
	if !bytes.Contains(raw, []byte(marker)) || !bytes.Contains(raw, []byte("reply_stream")) {
		t.Fatalf("missing destination terminal/identity %q: %s", marker, raw)
	}
}

func TestBridgeStreamingTruncationAndProviderErrorNeverComplete(t *testing.T) {
	tests := []struct {
		name string
		wire string
	}{
		{"truncated", `data: {"id":"r","choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n"},
		{"provider_error", `data: {"error":{"message":"private upstream diagnostic","code":503}}` + "\n\n"},
	}
	for _, tc := range tests {
		for _, destination := range bridgeProtocols {
			t.Run(tc.name+"_to_"+string(destination), func(t *testing.T) {
				proxy, _ := newBridgeProxy(t, destination, bridge.OpenAIChat, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, tc.wire)
				})
				status, raw, _ := callBridgePartial(t, proxy, destination, bridgeStreamingClientBody(destination, false))
				if status != http.StatusOK {
					t.Fatalf("status=%d body=%s", status, raw)
				}
				for _, marker := range []string{"data: [DONE]", `"type":"response.completed"`, `"type":"message_stop"`, `"finishReason":"STOP"`, `"finishReason":"OTHER"`} {
					if bytes.Contains(raw, []byte(marker)) {
						t.Fatalf("failed upstream emitted terminal marker %q: %s", marker, raw)
					}
				}
				if bytes.Contains(raw, []byte("private upstream diagnostic")) {
					t.Fatalf("failed upstream leaked diagnostics: %s", raw)
				}
			})
		}
	}
}

func callBridgePartial(t *testing.T, proxyURL *httptest.Server, p bridge.Protocol, body string) (int, []byte, error) {
	t.Helper()
	path, _, err := bridge.Endpoint(p, "client-model", true)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, proxyURL.URL+"/provider/p"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, readErr
}

func TestBridgeStreamingCancellationClosesUpstreamAndReleasesLease(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	var closeCanceled sync.Once
	proxy, srv := newBridgeProxy(t, bridge.Anthropic, bridge.OpenAIChat, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"r","model":"m","choices":[{"index":0,"delta":{"content":"partial"}}]}`+"\n\n")
		flusher.Flush()
		<-r.Context().Done()
		closeCanceled.Do(func() { close(upstreamCanceled) })
	})
	srv.rateLimiter.Update(0, 1)

	ctx, cancel := context.WithCancel(context.Background())
	path, _, _ := bridge.Endpoint(bridge.Anthropic, "client-model", true)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/provider/p"+path, strings.NewReader(bridgeStreamingClientBody(bridge.Anthropic, false)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	_, _ = resp.Body.Read(buf)
	cancel()
	_ = resp.Body.Close()
	select {
	case <-upstreamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("client cancellation did not close the bridged upstream stream")
	}

	deadline := time.Now().Add(3 * time.Second)
	for activeBridgeLimiterLeases(srv) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := activeBridgeLimiterLeases(srv); got != 0 {
		t.Fatalf("canceled bridge retained %d active limiter leases", got)
	}
}

func activeBridgeLimiterLeases(srv *Server) int {
	srv.rateLimiter.mu.Lock()
	defer srv.rateLimiter.mu.Unlock()
	active := 0
	for _, limiter := range srv.rateLimiter.limits {
		limiter.mu.Lock()
		active += limiter.active
		limiter.mu.Unlock()
	}
	return active
}
