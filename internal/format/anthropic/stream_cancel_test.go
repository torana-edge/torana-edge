package anthropic

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func TestSerializeCanceledClosedStreamDoesNotFinish(t *testing.T) {
	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		events := make(chan engine.StreamEvent)
		close(events)
		var out bytes.Buffer
		err := (&StreamAdapter{}).SerializeStream(ctx, &out, events)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d: error = %v, want context cancellation", i, err)
		}
		if strings.Contains(out.String(), "message_stop") {
			t.Fatalf("iteration %d: canceled stream emitted message_stop: %q", i, out.String())
		}
	}
}
