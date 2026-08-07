package anthropic

// AUDIT SCRATCH TESTS — not for commit.

import (
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// An SSE error frame with no "error" member panics ParseStream's goroutine.
// The goroutine has no recover, so this takes down the whole proxy process.
func TestAuditErrorFrameWithoutErrorMemberPanics(t *testing.T) {
	body := strings.NewReader("data: {\"type\":\"error\"}\n\n")
	s := &StreamAdapter{}
	for ev := range s.ParseStream(body) {
		t.Logf("event: %+v", ev)
	}
}

// A data line longer than bufio.Scanner's default 64KiB limit silently ends
// the stream: no error event, no scanner.Err() check — the pipeline sees a
// clean EOF and the client gets a truncated response that looks complete.
func TestAuditOversizedFrameSilentlyTruncates(t *testing.T) {
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
	t.Logf("events=%d sawFinish=%v sawError=%v", len(got), sawFinish, sawError)
	if !sawFinish && !sawError {
		t.Errorf("stream truncated silently: the finish frame after the oversized delta was dropped and no error was reported")
	}
}
