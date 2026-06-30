// Package engine defines the modular middleware pipeline that sits between
// the reverse proxy and the upstream provider. Every middleware module
// implements one or both hook interfaces; the Pipeline executes them in
// registration order on every request/response cycle.
package engine

import (
	"io"
	"log"
	"net/http"
)

// RequestHook is implemented by modules that need to inspect or mutate an
// outbound request before it reaches the upstream provider.
//
//   - req  – the outgoing HTTP request (headers, method, URL).
//   - body – the full request body bytes (may be nil for GET/HEAD).
//
// Return the (possibly modified) body bytes.  An error skips remaining
// request hooks and logs a warning but does NOT abort the request.
type RequestHook interface {
	Name() string
	BeforeRequest(req *http.Request, body []byte) ([]byte, error)
}

// ResponseHook is implemented by modules that need to inspect or wrap the
// upstream response body (e.g. SSE stream scanners, response loggers).
//
//   - resp         – the upstream response (status, headers).
//   - body         – the upstream response body reader.
//   - originalReq  – the request that triggered this response (after Director
//                    rewrites; URL points to upstream, headers are preserved).
//   - originalBody – the full request body sent upstream.
//
// Return the (possibly wrapped or replaced) body reader.  An error uses the
// original body and logs a warning.
type ResponseHook interface {
	Name() string
	AfterResponse(resp *http.Response, body io.ReadCloser,
		originalReq *http.Request, originalBody []byte) (io.ReadCloser, error)
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
// Each hook receives the body returned by (or passed through by) the previous
// hook.  On error the current body is forwarded to the next hook after
// logging the failure.
func (p *Pipeline) RunBeforeRequest(req *http.Request, body []byte) []byte {
	for _, h := range p.requestHooks {
		newBody, err := h.BeforeRequest(req, body)
		if err != nil {
			// Log and continue with existing body.
			log.Printf("[%s] before-request error: %v", h.Name(), err)
			continue
		}
		body = newBody
	}
	return body
}

// RunAfterResponse runs every registered ResponseHook in order.
// Each hook receives the body reader returned by the previous hook.
// On error the original body is passed through.
func (p *Pipeline) RunAfterResponse(resp *http.Response, body io.ReadCloser,
	originalReq *http.Request, originalBody []byte) io.ReadCloser {
	for _, h := range p.responseHooks {
		next, err := h.AfterResponse(resp, body, originalReq, originalBody)
		if err != nil {
			log.Printf("[%s] after-response error: %v", h.Name(), err)
			continue
		}
		body = next
	}
	return body
}
