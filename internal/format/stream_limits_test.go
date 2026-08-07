package format_test

import (
	"io"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/format/anthropic"
	"github.com/torana-edge/torana-edge/internal/format/bedrock"
	"github.com/torana-edge/torana-edge/internal/format/gemini"
	"github.com/torana-edge/torana-edge/internal/format/openai"
	"github.com/torana-edge/torana-edge/internal/format/streamio"
)

func TestProviderStreamsReportOversizedFrames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		stream format.StreamAdapter
		prefix string
	}{
		{"anthropic", &anthropic.StreamAdapter{}, "data: "},
		{"openai", &openai.StreamAdapter{}, "data: "},
		{"bedrock", &bedrock.Stream{}, ""},
		{"gemini", &gemini.StreamAdapter{}, "data: "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := tc.prefix + strings.Repeat("x", streamio.MaxFrameBytes+1) + "\n"
			var events []engine.StreamEvent
			for event := range tc.stream.ParseStream(strings.NewReader(body)) {
				events = append(events, event)
			}
			if len(events) != 1 || events[0].Error == nil {
				t.Fatalf("events = %+v, want one stream read error", events)
			}
		})
	}
}

func TestStreamScannerAcceptsBoundaryMinusNewline(t *testing.T) {
	scanner := streamio.NewScanner(io.LimitReader(
		strings.NewReader(strings.Repeat("x", streamio.MaxFrameBytes-1)+"\n"),
		streamio.MaxFrameBytes,
	))
	if !scanner.Scan() {
		t.Fatalf("Scan() = false: %v", scanner.Err())
	}
	if scanner.Err() != nil {
		t.Fatalf("Err() = %v", scanner.Err())
	}
}
