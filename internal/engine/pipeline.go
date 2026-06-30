// Package engine defines the modular middleware pipeline that sits between
// the reverse proxy and the upstream provider. Every middleware module
// implements one or both hook interfaces; the Pipeline executes them in
// registration order on every request/response cycle.
//
// Plugins work on canonical IR types (ChatRequest, StreamEvent), never on
// raw provider wire formats. Format adapters at the edges translate wire ↔ IR.
package engine

import (
	"io"
	"log"
	"net/http"
)

// RequestHook is implemented by modules that need to inspect or mutate a
// chat request before it reaches the upstream provider.
//
//   - req  – the outgoing HTTP request (headers, method, URL).
//   - chat – the canonical chat request parsed from the provider wire format.
//
// Return the (possibly modified) chat request. A nil return means "no change,
// use the original." An error skips remaining request hooks and logs a warning
// but does NOT abort the request.
type RequestHook interface {
	Name() string
	BeforeRequest(req *http.Request, chat *ChatRequest) (*ChatRequest, error)
}

// ResponseHook is implemented by modules that need to inspect or transform
// a streaming response before it reaches the client.
//
//   - resp   – the upstream response (status, headers).
//   - events – channel of StreamEvents parsed from the provider SSE stream.
//   - req    – the original HTTP request (after Director rewrites).
//   - chat   – the canonical chat request that was sent upstream.
//
// Return the (possibly transformed) event channel.  An error uses the
// original channel and logs a warning.
type ResponseHook interface {
	Name() string
	AfterResponse(resp *http.Response, events <-chan StreamEvent,
		req *http.Request, chat *ChatRequest) (<-chan StreamEvent, error)
}

// Pipeline is an ordered chain of middleware hooks.
// The zero value is safe to use.
type Pipeline struct {
	requestHooks  []RequestHook
	responseHooks []ResponseHook
}

// New creates an empty Pipeline.
func New() *Pipeline {
	return &Pipeline{}
}

// AddRequestHook appends a hook to the request chain.
func (p *Pipeline) AddRequestHook(h RequestHook) {
	p.requestHooks = append(p.requestHooks, h)
}

// AddResponseHook appends a hook to the response chain.
func (p *Pipeline) AddResponseHook(h ResponseHook) {
	p.responseHooks = append(p.responseHooks, h)
}

// RunBeforeRequest runs every registered RequestHook in order.
// Each hook receives the ChatRequest returned by (or passed through by) the
// previous hook.  On error the current chat is forwarded to the next hook
// after logging the failure.
func (p *Pipeline) RunBeforeRequest(req *http.Request, chat *ChatRequest) *ChatRequest {
	for _, h := range p.requestHooks {
		next, err := h.BeforeRequest(req, chat)
		if err != nil {
			log.Printf("[%s] before-request error: %v", h.Name(), err)
			continue
		}
		if next != nil {
			chat = next
		}
	}
	return chat
}

// RunAfterResponse runs every registered ResponseHook in order.
// Each hook receives the event channel returned by the previous hook.
// On error the original channel is passed through.
func (p *Pipeline) RunAfterResponse(resp *http.Response, events <-chan StreamEvent,
	req *http.Request, chat *ChatRequest) <-chan StreamEvent {
	for _, h := range p.responseHooks {
		next, err := h.AfterResponse(resp, events, req, chat)
		if err != nil {
			log.Printf("[%s] after-response error: %v", h.Name(), err)
			continue
		}
		events = next
	}
	return events
}

// Compile-time guard: the old io.ReadCloser import is no longer needed.
var _ = io.Discard
