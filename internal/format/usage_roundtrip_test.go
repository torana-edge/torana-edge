package format_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
)

// TestStreamUsageRoundTrip validates the wire-fidelity invariant for provider
// token usage: each format's parser surfaces the provider's usage frame as a
// StreamEvent{Usage}, and its serializer re-emits it in the format's native
// shape — so clients keep receiving the token accounting they depend on, and
// the host can meter tokens from the canonical event.
func TestStreamUsageRoundTrip(t *testing.T) {
	cases := []struct {
		format string
		// native upstream stream containing usage (10 in / 5 out)
		input string
		// usageBeforeFinish asserts parser event ordering (serializers that
		// embed usage in their finish frame need it first).
		usageBeforeFinish bool
		// wireMarkers must all appear in the re-serialized stream.
		wireMarkers []string
	}{
		{
			format: "openai",
			input: "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
				"data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
				"data: [DONE]\n\n",
			usageBeforeFinish: false, // OpenAI sends usage after the finish chunk
			wireMarkers: []string{
				`"prompt_tokens":10`,
				`"completion_tokens":5`,
				"data: [DONE]",
			},
		},
		{
			format: "anthropic",
			input: "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n" +
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
				"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":5}}\n\n" +
				"data: {\"type\":\"message_stop\"}\n\n",
			usageBeforeFinish: true,
			wireMarkers: []string{
				`"usage":{"input_tokens":10,"output_tokens":5}`,
				`"stop_reason":"end_turn"`,
			},
		},
		{
			format: "bedrock",
			input: `{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"hi"}}}` + "\n" +
				`{"messageStop":{"stopReason":"end_turn"}}` + "\n" +
				`{"metadata":{"usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15}}}` + "\n",
			usageBeforeFinish: false, // metadata trails messageStop
			wireMarkers: []string{
				`"metadata":{"usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15}}`,
			},
		},
		{
			format: "gemini",
			input: `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":13}}` + "\n" +
				`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}` + "\n",
			usageBeforeFinish: true, // last-seen counts win, emitted before finish
			wireMarkers: []string{
				`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}`,
				`"finishReason":"STOP"`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			f := format.Lookup(tc.format)
			if f == nil {
				t.Fatalf("format %q not registered", tc.format)
			}

			// --- parse: the native usage frame becomes a Usage event --------
			var events []engine.StreamEvent
			for ev := range f.Stream.ParseStream(strings.NewReader(tc.input)) {
				events = append(events, ev)
			}

			usageIdx, finishIdx := -1, -1
			for i, ev := range events {
				if ev.Usage != nil {
					if usageIdx != -1 {
						t.Fatalf("more than one Usage event emitted")
					}
					usageIdx = i
				}
				if ev.FinishReason != "" {
					finishIdx = i
				}
			}
			if usageIdx == -1 {
				t.Fatalf("no Usage event parsed; events: %+v", events)
			}
			u := events[usageIdx].Usage
			if u.InputTokens != 10 || u.OutputTokens != 5 {
				t.Fatalf("usage = %d/%d, want 10/5", u.InputTokens, u.OutputTokens)
			}
			if finishIdx == -1 {
				t.Fatal("no FinishReason event parsed")
			}
			if tc.usageBeforeFinish && usageIdx > finishIdx {
				t.Fatalf("usage (idx %d) must precede finish (idx %d) for %s", usageIdx, finishIdx, tc.format)
			}

			// --- serialize: the Usage event returns to the native shape -----
			ch := make(chan engine.StreamEvent, len(events))
			for _, ev := range events {
				ch <- ev
			}
			close(ch)
			var out bytes.Buffer
			if err := f.Stream.SerializeStream(&out, nil, ch); err != nil {
				t.Fatalf("serialize: %v", err)
			}
			for _, marker := range tc.wireMarkers {
				if !strings.Contains(out.String(), marker) {
					t.Errorf("serialized stream missing %q:\n%s", marker, out.String())
				}
			}
		})
	}
}

// TestOpenAISerializeFinishBeforeUsageBeforeDone pins the openai frame order:
// finish chunk, then usage chunk, then [DONE] — [DONE] must not terminate the
// stream before a trailing usage frame is written.
func TestOpenAISerializeFinishBeforeUsageBeforeDone(t *testing.T) {
	f := format.Lookup("openai")
	ch := make(chan engine.StreamEvent, 3)
	text := "hi"
	ch <- engine.StreamEvent{TextDelta: &text}
	ch <- engine.StreamEvent{FinishReason: "stop"}
	ch <- engine.StreamEvent{Usage: &engine.StreamUsage{InputTokens: 10, OutputTokens: 5}}
	close(ch)

	var out bytes.Buffer
	if err := f.Stream.SerializeStream(&out, nil, ch); err != nil {
		t.Fatalf("serialize: %v", err)
	}
	s := out.String()
	finish := strings.Index(s, `"finish_reason":"stop"`)
	usage := strings.Index(s, `"prompt_tokens":10`)
	done := strings.Index(s, "data: [DONE]")
	if finish == -1 || usage == -1 || done == -1 {
		t.Fatalf("missing frames (finish=%d usage=%d done=%d):\n%s", finish, usage, done, s)
	}
	if !(finish < usage && usage < done) {
		t.Fatalf("frame order wrong (finish=%d usage=%d done=%d):\n%s", finish, usage, done, s)
	}
}
