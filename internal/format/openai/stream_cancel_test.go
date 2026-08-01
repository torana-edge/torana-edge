package openai

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// A closed events channel and canceled context are both ready in production
// when enforcement aborts a serializer. The channel branch must never win and
// fabricate either OpenAI success marker.
func TestSerializeCanceledClosedStreamDoesNotFinish(t *testing.T) {
	for _, tc := range []struct {
		name   string
		base   context.Context
		forbid []string
	}{
		{"chat", context.Background(), []string{"[DONE]"}},
		{
			"responses",
			context.WithValue(context.Background(), engine.ChatRequestKey, &engine.ChatRequest{
				ProviderExtensions: map[string]any{"_openai_variant": "responses"},
			}),
			[]string{"[DONE]", "response.completed"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both select arms are ready. Repeat enough times to exercise both
			// selections; the post-loop guard must make every iteration cancel.
			for i := 0; i < 100; i++ {
				ctx, cancel := context.WithCancel(tc.base)
				cancel()
				events := make(chan engine.StreamEvent)
				close(events)
				var out bytes.Buffer
				err := (&StreamAdapter{}).SerializeStream(ctx, &out, events)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("iteration %d: error = %v, want context cancellation", i, err)
				}
				for _, marker := range tc.forbid {
					if strings.Contains(out.String(), marker) {
						t.Fatalf("iteration %d: canceled stream emitted success marker %q: %q", i, marker, out.String())
					}
				}
			}
		})
	}
}
