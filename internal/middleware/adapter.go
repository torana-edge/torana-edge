package middleware

import (
	"log"
	"net/http"
	"net/url"
)

// Adapter handles transport-level concerns: upstream host rewriting and
// request logging. It does not mutate the request body.
type Adapter struct {
	TargetHost string // e.g. "api.deepseek.com"
}

// NewAdapter creates an Adapter from a parsed upstream URL.
func NewAdapter(upstream *url.URL) *Adapter {
	return &Adapter{TargetHost: upstream.Host}
}

func (a *Adapter) Name() string { return "adapter" }

// BeforeRequest sets the Host header to the upstream target and logs the
// incoming request line. The body is returned unchanged.
func (a *Adapter) BeforeRequest(req *http.Request, body []byte) ([]byte, error) {
	req.Host = a.TargetHost
	log.Printf("→ %s %s", req.Method, req.URL.Path)
	return body, nil
}
