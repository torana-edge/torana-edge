package proxy

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
)

func TestAppendNoticeEventsTerminalDiscipline(t *testing.T) {
	cases := []struct {
		name, finish string
		tool, failed bool
		wantNotice   bool
		wantOutcome  string
	}{
		{"complete", "stop", false, false, true, "delivered"},
		{"tool", "tool_calls", true, false, false, "not_completed"},
		{"length", "length", false, false, false, "not_completed"},
		{"truncated", "", false, false, false, "truncated"},
		{"error after finish", "stop", false, true, false, "stream_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := make(chan engine.StreamEvent, 5)
			input <- engine.StreamEvent{BlockStart: &engine.BlockStart{Index: 2, Kind: engine.BlockKindText}}
			text := "answer"
			input <- engine.StreamEvent{TextDelta: &text}
			input <- engine.StreamEvent{BlockStop: &engine.BlockStop{Index: 2}}
			if tc.tool {
				input <- engine.StreamEvent{ToolCallStart: &engine.ToolCallStart{Index: 3, ID: "call", Name: "read"}}
			}
			if tc.finish != "" {
				input <- engine.StreamEvent{FinishReason: tc.finish}
			}
			if tc.failed {
				input <- engine.StreamEvent{Error: &engine.StreamError{Message: "provider error"}}
			}
			close(input)
			var events []engine.StreamEvent
			outcomes := []string{}
			for event := range appendNoticeEvents(context.Background(), input, "notice", func(outcome string) { outcomes = append(outcomes, outcome) }) {
				events = append(events, event)
			}
			if len(outcomes) != 1 || outcomes[0] != tc.wantOutcome {
				t.Fatalf("outcomes=%v, want %q", outcomes, tc.wantOutcome)
			}
			noticed := false
			for _, event := range events {
				if event.TextDelta != nil && *event.TextDelta == "notice" {
					noticed = true
				}
			}
			if noticed != tc.wantNotice {
				t.Fatalf("notice=%v, want %v", noticed, tc.wantNotice)
			}
			if tc.wantNotice {
				if len(events) < 4 || events[len(events)-4].BlockStart == nil || events[len(events)-4].BlockStart.Index != 3 || events[len(events)-1].FinishReason != "stop" {
					t.Fatalf("notice did not precede completion with a fresh block index: %+v", events)
				}
			}
		})
	}
}

func TestAppendNoticeEventsSerializesInClientShape(t *testing.T) {
	for _, name := range []string{"anthropic", "openai", "openai-responses", "gemini", "gemini-codeassist"} {
		t.Run(name, func(t *testing.T) {
			input := make(chan engine.StreamEvent, 5)
			input <- engine.StreamEvent{MessageStart: &engine.StreamMessageStart{Role: "assistant", ID: "message", Model: "test"}}
			input <- engine.StreamEvent{BlockStart: &engine.BlockStart{Index: 0, Kind: engine.BlockKindText}}
			answer := "answer"
			input <- engine.StreamEvent{TextDelta: &answer}
			input <- engine.StreamEvent{BlockStop: &engine.BlockStop{Index: 0}}
			input <- engine.StreamEvent{FinishReason: "stop"}
			close(input)
			var output bytes.Buffer
			ctx := context.Background()
			formatName := name
			if name == "openai-responses" {
				formatName = "openai"
				ctx = context.WithValue(ctx, engine.ChatRequestKey, &engine.ChatRequest{OpenAIVariant: engine.OpenAIResponses})
			}
			if err := format.Lookup(formatName).Stream.SerializeStream(ctx, &output, appendNoticeEvents(ctx, input, "notice")); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), "notice") || !strings.Contains(output.String(), "answer") {
				t.Fatalf("missing text in %s stream: %s", name, output.String())
			}
		})
	}
}
