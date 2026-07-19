// Package format defines the adapter interfaces that translate between
// provider wire formats and the canonical IR types. Each supported format
// (Anthropic Messages, OpenAI Chat Completions, AWS Bedrock Converse, etc.)
// has its own sub-package implementing these interfaces.
package format

import (
	"context"
	"io"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// RequestAdapter converts between raw JSON and canonical ChatRequest.
type RequestAdapter interface {
	// Unmarshal parses rawBody into a ChatRequest.
	Unmarshal(rawBody []byte) (*engine.ChatRequest, error)
	// Marshal converts a ChatRequest back to the wire format JSON.
	Marshal(chat *engine.ChatRequest) ([]byte, error)
}

// StreamAdapter converts between an SSE byte stream and a channel of StreamEvents.
type StreamAdapter interface {
	// ParseStream reads SSE from reader and emits StreamEvents to the returned channel.
	// The channel is closed when the stream ends or on error.
	ParseStream(body io.Reader) <-chan engine.StreamEvent
	// SerializeStream writes StreamEvents from the channel as SSE to writer.
	// Returns when the channel is closed or on write error.
	SerializeStream(ctx context.Context, w io.Writer, events <-chan engine.StreamEvent) error
}

// Format bundles both adapters under a name.
type Format struct {
	Name    string
	Request RequestAdapter
	Stream  StreamAdapter
}
