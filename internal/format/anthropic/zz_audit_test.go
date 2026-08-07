package anthropic

import (
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func TestErrorFrameWithoutErrorMemberFailsClosed(t *testing.T) {
	body := strings.NewReader("data: {\"type\":\"error\"}\n\n")
	s := &StreamAdapter{}
	var got []engine.StreamEvent
	for ev := range s.ParseStream(body) {
		got = append(got, ev)
	}
	if len(got) != 1 || got[0].Error == nil {
		t.Fatalf("events = %+v, want one terminal error", got)
	}
	if !strings.Contains(got[0].Error.Message, "missing error detail") {
		t.Fatalf("error = %q, want bounded missing-detail diagnostic", got[0].Error.Message)
	}
}

func TestLargeFrameDoesNotSilentlyTruncate(t *testing.T) {
	big := strings.Repeat("x", 70*1024)
	var sb strings.Builder
	sb.WriteString(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n")
	sb.WriteString(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + big + `"}}` + "\n\n")
	sb.WriteString(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n")

	s := &StreamAdapter{}
	var got []engine.StreamEvent
	for ev := range s.ParseStream(strings.NewReader(sb.String())) {
		got = append(got, ev)
	}
	sawFinish := false
	sawError := false
	for _, ev := range got {
		if ev.FinishReason != "" {
			sawFinish = true
		}
		if ev.Error != nil {
			sawError = true
		}
	}
	if !sawFinish || sawError {
		t.Fatalf("events=%d sawFinish=%v sawError=%v, want complete stream", len(got), sawFinish, sawError)
	}
}
