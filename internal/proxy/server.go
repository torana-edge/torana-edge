// Package proxy implements the Torana Edge reverse proxy engine.
//
// It sits between a developer agent harness (e.g., Claude Code) and a cloud
// LLM provider (e.g., Anthropic). In its base form it is a transparent
// pass-through; middleware phases layer on request mutation and response
// clamping without changing the core forwarding logic.
package proxy

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// Config holds everything needed to start the proxy server.
// It mirrors the values read from .env at startup.
type Config struct {
	// Port is the TCP port the proxy listens on (e.g. "8080").
	Port string

	// UpstreamURL is the base URL of the LLM provider
	// (e.g. "https://api.anthropic.com"). No trailing slash.
	UpstreamURL string

	// Provider is a human-readable label: "anthropic" or "openai".
	// It is used later to branch provider-specific logic.
	Provider string
}

// Server wraps the HTTP listener and the reverse-proxy plumbing.
type Server struct {
	config     Config
	proxy      *httputil.ReverseProxy
	httpServer *http.Server
}

// --- Construction -----------------------------------------------------------

// New builds a Server, validates the upstream URL, and wires the HTTP mux so
// every incoming request is forwarded through Go's standard single-host reverse
// proxy.
func New(cfg Config) (*Server, error) {
	if cfg.Port == "" {
		cfg.Port = "8080"
	}

	target, err := url.Parse(cfg.UpstreamURL)
	if err != nil {
		return nil, fmt.Errorf("proxy: invalid upstream URL %q: %w", cfg.UpstreamURL, err)
	}

	// NewSingleHostReverseProxy gives us a Director that rewrites the
	// outgoing request's Scheme, Host, and Path.  Because target has no
	// trailing path segment the original request path is preserved as-is,
	// so POST /v1/messages → POST https://api.anthropic.com/v1/messages.
	proxy := httputil.NewSingleHostReverseProxy(target)

	// --- future extension points (Phases 2-4) --------------------------
	// These are no-ops today.  Later phases will replace them with real
	// logic and the core forwarding loop stays untouched.
	//   - Director:   inject intent schemas, compact tool results
	//   - ModifyResponse: clamp SSE streams, strip injection keys
	//   - Transport:  add API-key header injection, TLS tuning
	// ------------------------------------------------------------------

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("→ %s %s", r.Method, r.URL.Path)
		proxy.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0,  // disabled – SSE streams are long-lived
		IdleTimeout:  120 * time.Second,
	}

	return &Server{
		config:     cfg,
		proxy:      proxy,
		httpServer: srv,
	}, nil
}

// --- Lifecycle --------------------------------------------------------------

// ListenAndServe starts the HTTP server on the configured address.
// It blocks until the server stops (either via Shutdown or a fatal error).
func (s *Server) ListenAndServe() error {
	log.Printf("Torana Edge → %s  upstream: %s (%s)", s.httpServer.Addr, s.config.UpstreamURL, s.config.Provider)
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("proxy: listen error: %w", err)
	}
	return nil
}

// Serve is the test-friendly variant: it accepts a pre-bound listener so
// callers can use port :0 and discover the actual address before calling.
func (s *Server) Serve(ln net.Listener) error {
	log.Printf("Torana Edge → %s  upstream: %s (%s)", ln.Addr(), s.config.UpstreamURL, s.config.Provider)
	if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("proxy: serve error: %w", err)
	}
	return nil
}

// Shutdown drains in-flight requests and stops the server.
// It accepts a context to cap how long the graceful drain may take.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}
