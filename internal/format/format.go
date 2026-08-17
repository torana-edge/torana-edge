// Package format defines the adapter interfaces that translate between
// provider wire formats and the canonical IR types. Each supported format
// (Anthropic Messages, OpenAI Chat Completions, AWS Bedrock Converse, etc.)
// has its own sub-package implementing these interfaces.
package format

import (
	"context"
	"io"
	"net/http"
	"strings"

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
	Name             string
	Request          RequestAdapter
	Stream           StreamAdapter
	MatchesInference func(method, path string) bool
}

// PostInferencePaths returns a matcher for POST inference endpoints identified
// by their complete path suffix. Providers commonly prepend deployment or API
// version segments, so matching the stable endpoint suffix preserves those
// layouts without treating a substring in the middle of an auxiliary path as
// inference. Query strings are not part of URL.Path and are intentionally
// irrelevant to endpoint classification.
func PostInferencePaths(suffixes ...string) func(method, path string) bool {
	owned := append([]string(nil), suffixes...)
	return func(method, path string) bool {
		if method != http.MethodPost {
			return false
		}
		path = strings.TrimSuffix(path, "/")
		for _, suffix := range owned {
			if suffix != "" && strings.HasSuffix(path, suffix) {
				return true
			}
		}
		return false
	}
}

// HandlesInference reports whether this format owns the request as an
// inference operation. Everything else must remain ordinary reverse-proxy
// traffic and must not be decoded into Torana's IR or sent through plugins.
func (f *Format) HandlesInference(method, path string) bool {
	return f != nil && f.MatchesInference != nil && f.MatchesInference(method, path)
}
