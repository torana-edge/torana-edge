package middleware

import (
	"log"
	"net/http"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// Adapter handles transport-level concerns: request logging.
// Host rewriting is handled by the proxy Director since the upstream
// is now resolved per-request from the provider configuration.
type Adapter struct{}

// NewAdapter creates an Adapter.
func NewAdapter() *Adapter {
	return &Adapter{}
}

func (a *Adapter) Name() string { return "adapter" }

// BeforeRequest logs the incoming request line. The chat request is
// returned unchanged.
func (a *Adapter) BeforeRequest(req *http.Request, chat *engine.ChatRequest) (*engine.ChatRequest, error) {
	log.Printf("→ %s %s", req.Method, req.URL.Path)
	return chat, nil
}
