package bedrock

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func TestSerializeCanceledClosedStreamDoesNotFlush(t *testing.T) {
	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		events := make(chan engine.StreamEvent)
		close(events)
		var out bytes.Buffer
		err := (&Stream{}).SerializeStream(ctx, &out, events)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d: error = %v, want context cancellation", i, err)
		}
		if out.Len() != 0 {
			t.Fatalf("iteration %d: canceled stream flushed output: %q", i, out.String())
		}
	}
}
