package bridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format/streamio"
)

func TestStreamBridgeTextMatrix(t *testing.T) {
	fixtures := map[Protocol]string{
		OpenAIChat: strings.Join([]string{
			`data: {"id":"source-id","model":"source-model","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
			`data: {"id":"source-id","model":"source-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"id":"source-id","model":"source-model","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":3}}}`,
			`data: [DONE]`,
		}, "\n\n"),
		OpenAIResponses: strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"source-id","model":"source-model","status":"in_progress"}}`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_0","type":"message","role":"assistant","content":[]}}`,
			`data: {"type":"response.output_text.delta","item_id":"msg_0","output_index":0,"content_index":0,"delta":"hello"}`,
			`data: {"type":"response.completed","response":{"id":"source-id","model":"source-model","status":"completed","output":[{"id":"msg_0","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`,
		}, "\n\n"),
		Anthropic: strings.Join([]string{
			`data: {"type":"message_start","message":{"id":"source-id","role":"assistant","model":"source-model","usage":{"input_tokens":7,"cache_read_input_tokens":3}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`data: {"type":"message_stop"}`,
		}, "\n\n"),
		Gemini: strings.Join([]string{
			`data: {"responseId":"source-id","modelVersion":"source-model","candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]}}]}`,
			`data: {"responseId":"source-id","modelVersion":"source-model","candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"totalTokenCount":12,"cachedContentTokenCount":3}}`,
		}, "\n\n"),
		GeminiCodeAssist: strings.Join([]string{
			`data: {"response":{"responseId":"source-id","modelVersion":"source-model","candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]}}]}}`,
			`data: {"response":{"responseId":"source-id","modelVersion":"source-model","candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"totalTokenCount":12,"cachedContentTokenCount":3}}}`,
		}, "\n\n"),
	}
	protocols := []Protocol{OpenAIChat, OpenAIResponses, Anthropic, Gemini, GeminiCodeAssist}
	usageOptions, err := engine.ParseOptionalJSONObject([]byte(`{"stream_options":{"include_usage":true}}`))
	if err != nil {
		t.Fatal(err)
	}

	for _, from := range protocols {
		for _, to := range protocols {
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				events := collectEvents(ParseStream(from, strings.NewReader(fixtures[from])))
				sourceInput := 10
				if from == Anthropic {
					sourceInput = 7
				}
				assertSuccessfulEvents(t, events, "source-id", "hello", sourceInput, 2)

				input := replayEvents(events)
				chat := &engine.ChatRequest{ProviderExtensions: usageOptions}
				var wire bytes.Buffer
				if err := SerializeStream(context.Background(), &wire, input, from, to, chat); err != nil {
					t.Fatalf("SerializeStream: %v", err)
				}
				roundTrip := collectEvents(ParseStream(to, strings.NewReader(wire.String())))
				destinationInput := 10
				if to == Anthropic {
					destinationInput = 7
				}
				assertSuccessfulEvents(t, roundTrip, "source-id", "hello", destinationInput, 2)
			})
		}
	}
}

func TestParseStreamDelaysOpenAIFinishUntilLateUsage(t *testing.T) {
	wire := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"x"}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4}}`,
		`data: [DONE]`,
	}, "\n\n")
	events := collectEvents(ParseStream(OpenAIChat, strings.NewReader(wire)))
	if len(events) < 3 || events[len(events)-2].Usage == nil || events[len(events)-1].FinishReason != "stop" {
		t.Fatalf("terminal events = %+v, want usage then finish", events)
	}
}

func TestParseStreamRejectsTruncatedEOFForEveryProtocol(t *testing.T) {
	cases := map[Protocol]string{
		OpenAIChat: `data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}`,
		OpenAIResponses: strings.Join([]string{
			`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"m","type":"message","role":"assistant","content":[]}}`,
			`data: {"type":"response.output_text.delta","item_id":"m","output_index":0,"content_index":0,"delta":"partial"}`,
		}, "\n\n"),
		Anthropic:        `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		Gemini:           `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}]}`,
		GeminiCodeAssist: `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}]}}`,
	}
	for protocol, wire := range cases {
		t.Run(string(protocol), func(t *testing.T) {
			events := collectEvents(ParseStream(protocol, strings.NewReader(wire)))
			last := events[len(events)-1]
			if last.Error == nil || !strings.Contains(last.Error.Message, "terminal marker") {
				t.Fatalf("events = %+v, want truncated terminal error", events)
			}
			for _, event := range events {
				if event.FinishReason != "" {
					t.Fatalf("truncated stream emitted successful finish: %+v", events)
				}
			}
		})
	}
}

func TestParseStreamPreservesInterleavedOpenAIToolCalls(t *testing.T) {
	wire := strings.Join([]string{
		`data: {"id":"r","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"one"}},{"index":1,"id":"b","function":{"name":"two"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"b\":"}},{"index":0,"function":{"arguments":"{\"a\":"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}},{"index":1,"function":{"arguments":"2}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		`data: [DONE]`,
	}, "\n\n")
	events := collectEvents(ParseStream(OpenAIChat, strings.NewReader(wire)))
	var fragments = map[int]string{}
	var ends []int
	for _, event := range events {
		if event.ToolCallDelta != nil {
			fragments[event.ToolCallDelta.Index] += event.ToolCallDelta.ArgumentsDelta
		}
		if event.ToolCallEnd != nil {
			ends = append(ends, event.ToolCallEnd.Index)
		}
	}
	if fragments[0] != `{"a":1}` || fragments[1] != `{"b":2}` || len(ends) != 2 || ends[0] != 0 || ends[1] != 1 {
		t.Fatalf("fragments=%v ends=%v events=%+v", fragments, ends, events)
	}
}

func TestSerializeStreamAnthropicRequiresUsageAndPreservesExplicitZero(t *testing.T) {
	var missing bytes.Buffer
	err := SerializeStream(context.Background(), &missing, replayEvents([]engine.StreamEvent{{FinishReason: "stop"}}), Gemini, Anthropic, &engine.ChatRequest{})
	if err == nil || !strings.Contains(err.Error(), "requires terminal stream usage") || strings.Contains(missing.String(), `"type":"message_stop"`) {
		t.Fatalf("missing usage error=%v wire=%s", err, missing.String())
	}

	zero := &engine.StreamUsage{}
	var present bytes.Buffer
	err = SerializeStream(context.Background(), &present, replayEvents([]engine.StreamEvent{{Usage: zero}, {FinishReason: "stop"}}), Gemini, Anthropic, &engine.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	events := collectEvents(ParseStream(Anthropic, strings.NewReader(present.String())))
	if len(events) < 2 || events[len(events)-2].Usage == nil || events[len(events)-1].FinishReason != "stop" {
		t.Fatalf("explicit zero usage lost: events=%+v wire=%s", events, present.String())
	}
}

func TestStreamBridgeInterleavedToolsToEveryDestination(t *testing.T) {
	wire := strings.Join([]string{
		`data: {"id":"tool-response","model":"source-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-a","function":{"name":"one"}},{"index":1,"id":"call-b","function":{"name":"two"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"b\":"}},{"index":0,"function":{"arguments":"{\"a\":"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}},{"index":1,"function":{"arguments":"2}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		`data: [DONE]`,
	}, "\n\n")
	events := collectEvents(ParseStream(OpenAIChat, strings.NewReader(wire)))
	for _, to := range []Protocol{OpenAIChat, OpenAIResponses, Anthropic, Gemini, GeminiCodeAssist} {
		t.Run(string(to), func(t *testing.T) {
			var output bytes.Buffer
			if err := SerializeStream(context.Background(), &output, replayEvents(events), OpenAIChat, to, &engine.ChatRequest{}); err != nil {
				t.Fatal(err)
			}
			roundTrip := collectEvents(ParseStream(to, strings.NewReader(output.String())))
			calls := map[string]string{}
			indexToID := map[int]string{}
			finish := ""
			for _, event := range roundTrip {
				if event.Error != nil {
					t.Fatalf("parse serialized stream: %s\n%s", event.Error.Message, output.String())
				}
				if event.ToolCallStart != nil {
					indexToID[event.ToolCallStart.Index] = event.ToolCallStart.ID
				}
				if event.ToolCallDelta != nil {
					calls[indexToID[event.ToolCallDelta.Index]] += event.ToolCallDelta.ArgumentsDelta
				}
				if event.FinishReason != "" {
					finish = event.FinishReason
				}
			}
			if calls["call-a"] != `{"a":1}` || calls["call-b"] != `{"b":2}` || finish != "tool_calls" {
				t.Fatalf("calls=%v finish=%q events=%+v\n%s", calls, finish, roundTrip, output.String())
			}
		})
	}
}

func TestSerializeStreamUsageNormalizationAndOpenAIOptIn(t *testing.T) {
	usage := &engine.StreamUsage{InputTokens: 7, OutputTokens: 2, CacheReadTokens: 3}
	makeEvents := func() <-chan engine.StreamEvent {
		ch := make(chan engine.StreamEvent, 2)
		ch <- engine.StreamEvent{Usage: usage}
		ch <- engine.StreamEvent{FinishReason: "stop"}
		close(ch)
		return ch
	}

	var without bytes.Buffer
	if err := SerializeStream(context.Background(), &without, makeEvents(), Anthropic, OpenAIChat, &engine.ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(without.String(), `"usage"`) {
		t.Fatalf("usage emitted without client opt-in: %s", without.String())
	}
	if usage.InputTokens != 7 {
		t.Fatalf("source usage mutated: %+v", usage)
	}

	ext, _ := engine.ParseOptionalJSONObject([]byte(`{"stream_options":{"include_usage":true}}`))
	var with bytes.Buffer
	if err := SerializeStream(context.Background(), &with, makeEvents(), Anthropic, OpenAIChat, &engine.ChatRequest{ProviderExtensions: ext}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(with.String(), `"prompt_tokens":10`) {
		t.Fatalf("Anthropic input units not normalized: %s", with.String())
	}
	finishAt := strings.Index(with.String(), `"finish_reason":"stop"`)
	usageAt := strings.Index(with.String(), `"prompt_tokens":10`)
	doneAt := strings.Index(with.String(), `[DONE]`)
	if !(finishAt >= 0 && finishAt < usageAt && usageAt < doneAt) {
		t.Fatalf("OpenAI terminal order wrong: %s", with.String())
	}
}

func TestSerializeStreamRejectsInvalidOrUnrepresentableUsage(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	tests := []struct {
		name  string
		from  Protocol
		to    Protocol
		usage engine.StreamUsage
		chat  *engine.ChatRequest
		want  string
	}{
		{"negative", OpenAIChat, Anthropic, engine.StreamUsage{InputTokens: -1}, &engine.ChatRequest{}, "non-negative"},
		{"total overflow", OpenAIChat, Anthropic, engine.StreamUsage{InputTokens: maxInt, OutputTokens: 1}, &engine.ChatRequest{}, "overflows"},
		{"anthropic input overflow", Anthropic, OpenAIResponses, engine.StreamUsage{InputTokens: maxInt, CacheReadTokens: 1}, &engine.ChatRequest{}, "overflows"},
		{"inclusive cache exceeds input", Gemini, Anthropic, engine.StreamUsage{InputTokens: 1, CacheReadTokens: 2}, &engine.ChatRequest{}, "exceeds"},
		{"gemini cache write", Anthropic, Gemini, engine.StreamUsage{InputTokens: 4, CacheWriteTokens: 1}, &engine.ChatRequest{}, "cache write"},
	}
	ext, _ := engine.ParseOptionalJSONObject([]byte(`{"stream_options":{"include_usage":true}}`))
	tests = append(tests, struct {
		name  string
		from  Protocol
		to    Protocol
		usage engine.StreamUsage
		chat  *engine.ChatRequest
		want  string
	}{"chat cache write", Anthropic, OpenAIChat, engine.StreamUsage{InputTokens: 4, CacheWriteTokens: 1}, &engine.ChatRequest{ProviderExtensions: ext}, "cache write"})
	for _, tc := range tests {
		events := []engine.StreamEvent{{Usage: &tc.usage}, {FinishReason: "stop"}}
		err := SerializeStream(context.Background(), &bytes.Buffer{}, replayEvents(events), tc.from, tc.to, tc.chat)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error=%v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestParseStreamRejectsMalformedProviderUsage(t *testing.T) {
	tests := []struct {
		protocol Protocol
		wire     string
	}{
		{OpenAIChat, strings.Join([]string{
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":99}}`,
			`data: [DONE]`,
		}, "\n\n")},
		{OpenAIResponses, `data: {"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":99}}}`},
		{Anthropic, strings.Join([]string{
			`data: {"type":"message_start","message":{"id":"r","role":"assistant","model":"m","usage":{"input_tokens":"two"}}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			`data: {"type":"message_stop"}`,
		}, "\n\n")},
		{Gemini, `data: {"candidates":[{"index":0,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":99}}`},
	}
	for _, tc := range tests {
		t.Run(string(tc.protocol), func(t *testing.T) {
			events := collectEvents(ParseStream(tc.protocol, strings.NewReader(tc.wire)))
			if len(events) == 0 || events[len(events)-1].Error == nil || !strings.Contains(events[len(events)-1].Error.Message, "invalid") {
				t.Fatalf("events=%+v, want invalid usage error", events)
			}
		})
	}
}

func TestSerializeStreamRejectsUnrepresentableEvents(t *testing.T) {
	tests := []struct {
		name   string
		from   Protocol
		to     Protocol
		events []engine.StreamEvent
		want   string
	}{
		{"provider block", Anthropic, Gemini, []engine.StreamEvent{{BlockStart: &engine.BlockStart{Kind: engine.BlockKindProvider, ProviderKind: "redacted"}}, {FinishReason: "stop"}}, "provider-specific"},
		{"cross-protocol thinking", Anthropic, Gemini, []engine.StreamEvent{{BlockStart: &engine.BlockStart{Kind: engine.BlockKindThinking, Index: 0}}, {FinishReason: "stop"}}, "thinking"},
		{"cross-protocol signature", Anthropic, Gemini, []engine.StreamEvent{{SignatureDelta: new("provider-issued")}, {FinishReason: "stop"}}, "signatures"},
		{"signature", Gemini, OpenAIResponses, []engine.StreamEvent{{SignatureDelta: new("sig")}, {FinishReason: "stop"}}, "signatures"},
		{"freeform", OpenAIResponses, Anthropic, []engine.StreamEvent{{ToolCallStart: &engine.ToolCallStart{ID: "c", Name: "shell", InvocationKind: engine.ToolInvocationFreeform}}, {FinishReason: "tool_calls"}}, "free-form"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := SerializeStream(context.Background(), &bytes.Buffer{}, replayEvents(tc.events), tc.from, tc.to, &engine.ChatRequest{})
			var unsupportedErr *UnsupportedError
			if !errors.As(err, &unsupportedErr) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want UnsupportedError containing %q", err, tc.want)
			}
		})
	}
}

func TestParseStreamRejectsOpenAIChatMultipleOrNonzeroChoices(t *testing.T) {
	tests := []string{
		`data: {"choices":[{"index":0,"delta":{"content":"a"}},{"index":1,"delta":{"content":"b"}}]}`,
		`data: {"choices":[{"index":1,"delta":{"content":"b"}}]}`,
	}
	for _, wire := range tests {
		events := collectEvents(ParseStream(OpenAIChat, strings.NewReader(wire+"\n\ndata: [DONE]\n\n")))
		if len(events) == 0 || events[len(events)-1].Error == nil || !strings.Contains(events[len(events)-1].Error.Message, "does not support") {
			t.Fatalf("events = %+v, want unsupported choice error", events)
		}
	}
}

func TestStreamBridgeRejectsMalformedToolArguments(t *testing.T) {
	events := []engine.StreamEvent{
		{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "c", Name: "f"}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `[]`}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 0}},
		{FinishReason: "tool_calls"},
	}
	err := SerializeStream(context.Background(), &bytes.Buffer{}, replayEvents(events), OpenAIChat, Anthropic, &engine.ChatRequest{})
	if err == nil || !strings.Contains(err.Error(), "complete JSON object") {
		t.Fatalf("SerializeStream error = %v", err)
	}

	wire := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"f","arguments":"{\"x\":"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n")
	parsed := collectEvents(ParseStream(OpenAIChat, strings.NewReader(wire)))
	if len(parsed) == 0 || parsed[len(parsed)-1].Error == nil || !strings.Contains(parsed[len(parsed)-1].Error.Message, "complete JSON object") {
		t.Fatalf("ParseStream events = %+v", parsed)
	}
}

func TestSerializeStreamCapsCumulativeToolArguments(t *testing.T) {
	fragment := strings.Repeat("x", streamio.MaxFrameBytes/2+1)
	events := []engine.StreamEvent{
		{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "c", Name: "f"}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: fragment}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: fragment}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 0}},
		{FinishReason: "tool_calls"},
	}
	err := SerializeStream(context.Background(), &bytes.Buffer{}, replayEvents(events), OpenAIChat, Gemini, &engine.ChatRequest{})
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("SerializeStream error = %v", err)
	}
}

func TestParseStreamEmitsSanitizedProviderErrorPromptly(t *testing.T) {
	reader, writer := io.Pipe()
	events := ParseStream(OpenAIChat, reader)
	go func() {
		_, _ = io.WriteString(writer, `data: {"error":{"message":"sensitive provider detail","code":503}}`+"\n\n")
	}()
	select {
	case event := <-events:
		if event.Error == nil || strings.Contains(event.Error.Message, "sensitive") || !strings.Contains(event.Error.Message, "upstream openai-chat stream failed") {
			t.Fatalf("event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("provider error was held until upstream EOF")
	}
	_ = writer.Close()
	collectEvents(events)
}

func TestParseStreamGeneratesResponseScopedCallIDs(t *testing.T) {
	wire := `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"f","args":{"x":1}}}]},"finishReason":"STOP"}]}`
	var responseIDs, callIDs []string
	for range 2 {
		for _, event := range collectEvents(ParseStream(Gemini, strings.NewReader(wire))) {
			if event.MessageStart != nil {
				responseIDs = append(responseIDs, event.MessageStart.ID)
			}
			if event.ToolCallStart != nil {
				callIDs = append(callIDs, event.ToolCallStart.ID)
			}
			if event.Error != nil {
				t.Fatal(event.Error.Message)
			}
		}
	}
	if len(responseIDs) != 2 || len(callIDs) != 2 || responseIDs[0] == responseIDs[1] || callIDs[0] == callIDs[1] {
		t.Fatalf("response IDs=%v call IDs=%v", responseIDs, callIDs)
	}
}

func TestParseStreamRejectsResponsesTerminalThatHidesOutput(t *testing.T) {
	tests := []string{
		`data: {"type":"response.completed","response":{"status":"failed"}}`,
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"id":"secret","type":"computer_call"}]}}`,
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"id":"msg","type":"message","content":[{"type":"output_text","text":"hidden"}]}]}}`,
		strings.Join([]string{
			`data: {"type":"response.output_item.added","item":{"id":"msg","type":"message","status":"in_progress"}}`,
			`data: {"type":"response.completed","response":{"status":"completed","output":[{"id":"msg","type":"message","status":"failed","content":[{"type":"output_text","text":"hidden"}]}]}}`,
		}, "\n\n"),
	}
	for _, wire := range tests {
		events := collectEvents(ParseStream(OpenAIResponses, strings.NewReader(wire)))
		if len(events) == 0 || events[len(events)-1].Error == nil {
			t.Fatalf("events = %+v, want terminal validation error", events)
		}
		for _, event := range events {
			if event.FinishReason != "" {
				t.Fatalf("invalid terminal emitted success: %+v", events)
			}
		}
	}
}

func TestParseStreamRejectsDataAfterTerminal(t *testing.T) {
	wire := "data: [DONE]\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"late\"}}]}\n\n"
	events := collectEvents(ParseStream(OpenAIChat, strings.NewReader(wire)))
	if len(events) == 0 || events[len(events)-1].Error == nil || !strings.Contains(events[len(events)-1].Error.Message, "followed its terminal") {
		t.Fatalf("events = %+v", events)
	}
}

func TestParseStreamRejectsChangingResponseAndToolIDs(t *testing.T) {
	tests := []string{
		strings.Join([]string{
			`data: {"id":"first","model":"m","choices":[{"index":0,"delta":{"content":"a"}}]}`,
			`data: {"id":"second","model":"m","choices":[{"index":0,"delta":{"content":"b"},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}, "\n\n"),
		strings.Join([]string{
			`data: {"id":"r","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"first","function":{"name":"weather"}}]}}]}`,
			`data: {"id":"r","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"second","function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}, "\n\n"),
	}
	for _, wire := range tests {
		events := collectEvents(ParseStream(OpenAIChat, strings.NewReader(wire)))
		if len(events) == 0 || events[len(events)-1].Error == nil || !strings.Contains(events[len(events)-1].Error.Message, "changed") {
			t.Fatalf("events = %+v, want changing identity error", events)
		}
	}
}

func TestSerializeStreamValidatesToolIdentityForDestination(t *testing.T) {
	events := []engine.StreamEvent{
		{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "call_1", Name: "1invalid"}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{}`}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 0}},
		{FinishReason: "tool_calls"},
	}
	err := SerializeStream(context.Background(), &bytes.Buffer{}, replayEvents(events), OpenAIChat, Gemini, &engine.ChatRequest{})
	if err == nil || !strings.Contains(err.Error(), "tool-call name") {
		t.Fatalf("SerializeStream error = %v", err)
	}
}

func TestExportSDKValidationStreamFixtures(t *testing.T) {
	dir := os.Getenv("TORANA_BRIDGE_SDK_FIXTURE_DIR")
	if dir == "" {
		t.Skip("TORANA_BRIDGE_SDK_FIXTURE_DIR is not set")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "sdk fixture"
	usage := &engine.StreamUsage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 3}
	events := []engine.StreamEvent{
		{MessageStart: &engine.StreamMessageStart{ID: "resp_sdk_fixture", Model: "sdk-model", Role: "assistant"}},
		{TextDelta: &text},
		{Usage: usage},
		{FinishReason: "stop"},
	}
	includeUsage, _ := engine.ParseOptionalJSONObject([]byte(`{"stream_options":{"include_usage":true}}`))
	fixtures := []struct {
		name string
		to   Protocol
		chat *engine.ChatRequest
	}{
		{"openai-chat.sse", OpenAIChat, &engine.ChatRequest{Model: "sdk-model", Stream: true, ProviderExtensions: includeUsage}},
		{"openai-responses.sse", OpenAIResponses, &engine.ChatRequest{Model: "sdk-model", Stream: true, OpenAIVariant: engine.OpenAIResponses}},
		{"anthropic.sse", Anthropic, &engine.ChatRequest{Model: "sdk-model", Stream: true}},
	}
	for _, fixture := range fixtures {
		var wire bytes.Buffer
		if err := SerializeStream(context.Background(), &wire, replayEvents(events), Gemini, fixture.to, fixture.chat); err != nil {
			t.Fatalf("%s: %v", fixture.name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, fixture.name), wire.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	toolEvents := []engine.StreamEvent{
		{MessageStart: &engine.StreamMessageStart{ID: "resp_sdk_tool_fixture", Model: "sdk-model", Role: "assistant"}},
		{ToolCallStart: &engine.ToolCallStart{Index: 0, ID: "call_sdk_alpha", Name: "alpha"}},
		{ToolCallStart: &engine.ToolCallStart{Index: 1, ID: "call_sdk_beta", Name: "beta"}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 1, ArgumentsDelta: `{"n":`}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `{"n":`}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 0, ArgumentsDelta: `9007199254740993}`}},
		{ToolCallDelta: &engine.ToolCallDelta{Index: 1, ArgumentsDelta: `9007199254740995}`}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 0}},
		{ToolCallEnd: &engine.ToolCallEnd{Index: 1}},
		{Usage: usage},
		{FinishReason: "tool_calls"},
	}
	for _, fixture := range fixtures {
		name := strings.TrimSuffix(fixture.name, ".sse") + "-tool.sse"
		var wire bytes.Buffer
		if err := SerializeStream(context.Background(), &wire, replayEvents(toolEvents), Gemini, fixture.to, fixture.chat); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), wire.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestParseStreamRejectsProviderSpecificWireBlocks(t *testing.T) {
	tests := []struct {
		name     string
		protocol Protocol
		wire     string
	}{
		{"anthropic", Anthropic, strings.Join([]string{
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"secret"}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
			`data: {"type":"message_stop"}`,
		}, "\n\n")},
		{"responses", OpenAIResponses, strings.Join([]string{
			`data: {"type":"response.refusal.delta","delta":"no"}`,
			`data: {"type":"response.completed","response":{"status":"completed"}}`,
		}, "\n\n")},
		{"gemini", Gemini, `data: {"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]},"finishReason":"STOP"}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := collectEvents(ParseStream(tc.protocol, strings.NewReader(tc.wire)))
			if len(events) == 0 || events[len(events)-1].Error == nil || !strings.Contains(events[len(events)-1].Error.Message, "does not support") {
				t.Fatalf("events = %+v, want unsupported terminal error", events)
			}
			for _, event := range events {
				if event.FinishReason != "" {
					t.Fatalf("unsupported source emitted success: %+v", events)
				}
			}
		})
	}
}

func TestSerializeStreamErrorDoesNotEmitSuccessMarker(t *testing.T) {
	for _, to := range []Protocol{OpenAIChat, OpenAIResponses, Anthropic, Gemini, GeminiCodeAssist} {
		t.Run(string(to), func(t *testing.T) {
			events := make(chan engine.StreamEvent, 2)
			text := "partial"
			events <- engine.StreamEvent{TextDelta: &text}
			events <- engine.StreamEvent{Error: &engine.StreamError{Code: 503, Message: "upstream failed"}}
			close(events)
			var output bytes.Buffer
			err := SerializeStream(context.Background(), &output, events, OpenAIChat, to, &engine.ChatRequest{})
			if err == nil {
				t.Fatal("SerializeStream succeeded on error event")
			}
			for _, marker := range []string{"data: [DONE]", `"type":"response.completed"`, `"type":"message_stop"`, `"finishReason":"STOP"`, `"finishReason":"OTHER"`} {
				if strings.Contains(output.String(), marker) {
					t.Fatalf("error stream emitted success marker %q: %s", marker, output.String())
				}
			}
		})
	}
}

func TestSerializeStreamCancellationDoesNotEmitSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan engine.StreamEvent)
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() { done <- SerializeStream(ctx, &out, events, Gemini, Anthropic, &engine.ChatRequest{}) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SerializeStream leaked after cancellation")
	}
	if strings.Contains(out.String(), "message_stop") {
		t.Fatalf("canceled bridge emitted success marker: %s", out.String())
	}
}

func collectEvents(ch <-chan engine.StreamEvent) []engine.StreamEvent {
	var events []engine.StreamEvent
	for event := range ch {
		events = append(events, event)
	}
	return events
}

func replayEvents(events []engine.StreamEvent) <-chan engine.StreamEvent {
	ch := make(chan engine.StreamEvent, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return ch
}

func assertSuccessfulEvents(t *testing.T, events []engine.StreamEvent, wantID, wantText string, wantInput, wantOutput int) {
	t.Helper()
	var id, text, finish string
	var usage *engine.StreamUsage
	for _, event := range events {
		switch {
		case event.Error != nil:
			t.Fatalf("stream error: %s; events=%+v", event.Error.Message, events)
		case event.MessageStart != nil:
			id = event.MessageStart.ID
		case event.TextDelta != nil:
			text += *event.TextDelta
		case event.Usage != nil:
			usage = event.Usage
		case event.FinishReason != "":
			finish = event.FinishReason
		}
	}
	if id != wantID || text != wantText || finish != "stop" || usage == nil || usage.InputTokens != wantInput || usage.OutputTokens != wantOutput {
		t.Fatalf("id=%q text=%q finish=%q usage=%+v events=%+v", id, text, finish, usage, events)
	}
}
