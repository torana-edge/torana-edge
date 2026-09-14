package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func syntheticToolResponse() *pb.SyntheticResponse {
	return &pb.SyntheticResponse{Message: &pb.ResponseMessage{Blocks: []*pb.ResponseBlock{
		{Kind: &pb.ResponseBlock_Text{Text: &pb.ResponseTextBlock{Text: "before"}}},
		{Kind: &pb.ResponseBlock_ToolCall{ToolCall: &pb.ToolCall{Name: "first", ArgumentsJson: []byte(`{"n":9007199254740993,"d":1.00}`)}}},
		{Kind: &pb.ResponseBlock_ToolCall{ToolCall: &pb.ToolCall{Name: "second", ArgumentsJson: []byte(`{}`)}}},
	}}, FinishReason: "tool_calls"}
}
func TestRenderRespondCanonicalToolsAcrossFormats(t *testing.T) {
	for _, tc := range []struct {
		name, provider        string
		responses, codeassist bool
	}{{"chat", "openai", false, false}, {"responses", "openai", true, false}, {"anthropic", "anthropic", false, false}, {"gemini", "gemini", false, false}, {"codeassist", "gemini", false, true}} {
		for _, stream := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/json", true: "/stream"}[stream], func(t *testing.T) {
				chat := &engine.ChatRequest{Model: "m", Stream: stream, CodeAssist: tc.codeassist}
				if tc.responses {
					chat.OpenAIVariant = engine.OpenAIResponses
				}
				response := syntheticToolResponse()
				rendered, err := renderRespond(context.Background(), format.Lookup(tc.provider), chat, &wasm.RespondVerdict{Response: response})
				if err != nil {
					t.Fatal(err)
				}
				if rendered.Status != 200 || !bytes.Contains(rendered.Body, []byte("9007199254740993")) || !bytes.Contains(rendered.Body, []byte("1.00")) {
					t.Fatalf("lost argument lexemes: %s", rendered.Body)
				}
				if response.Message.Blocks[1].GetToolCall().Id != "" {
					t.Fatal("renderer mutated guest response")
				}
				if !stream {
					var obj map[string]any
					if err = json.Unmarshal(rendered.Body, &obj); err != nil {
						t.Fatal(err)
					}
					if tc.codeassist && obj["response"] == nil {
						t.Fatal("missing CodeAssist wrapper")
					}
				} else {
					starts, ends := 0, 0
					ids := map[string]bool{}
					args := map[int]string{}
					var text string
					for event := range format.Lookup(tc.provider).Stream.ParseStream(bytes.NewReader(rendered.Body)) {
						if event.Error != nil {
							t.Fatalf("synthetic stream parse: %v; %s", event.Error, rendered.Body)
						}
						if event.TextDelta != nil {
							text += *event.TextDelta
						}
						if event.ToolCallStart != nil {
							starts++
							id := event.ToolCallStart.ID
							if id == "" || ids[id] {
								t.Fatalf("missing/duplicate host ID: %q", id)
							}
							ids[id] = true
						}
						if event.ToolCallDelta != nil {
							args[event.ToolCallDelta.Index] += event.ToolCallDelta.ArgumentsDelta
						}
						if event.ToolCallEnd != nil {
							ends++
						}
					}
					if starts != 2 || ends != 2 || text != "before" {
						t.Fatalf("stream topology lost: starts=%d ends=%d text=%q; %s", starts, ends, text, rendered.Body)
					}
					found := false
					for _, value := range args {
						if value == `{"n":9007199254740993,"d":1.00}` {
							found = true
						}
					}
					if !found {
						t.Fatalf("tool argument bytes changed: %v", args)
					}
				}
			})
		}
	}
}
func TestRenderRespondRejectsUnrepresentableOrderAndCancellation(t *testing.T) {
	response := syntheticToolResponse()
	response.Message.Blocks = append(response.Message.Blocks, &pb.ResponseBlock{Kind: &pb.ResponseBlock_Text{Text: &pb.ResponseTextBlock{Text: "after"}}})
	for _, stream := range []bool{false, true} {
		_, err := renderRespond(context.Background(), format.Lookup("openai"), &engine.ChatRequest{Stream: stream}, &wasm.RespondVerdict{Response: response})
		if err == nil || !strings.Contains(err.Error(), "text after") {
			t.Fatalf("unrepresentable order accepted: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := renderRespond(ctx, format.Lookup("anthropic"), &engine.ChatRequest{Stream: true}, &wasm.RespondVerdict{Response: response}); err != context.Canceled {
		t.Fatalf("cancellation ignored: %v", err)
	}
}
