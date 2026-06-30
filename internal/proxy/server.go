// Package proxy implements the Torana Edge reverse proxy engine.
//
// It sits between a developer agent harness (e.g., Claude Code) and a cloud
// LLM provider. A modular middleware pipeline (internal/engine) intercepts
// every request/response pair — modules can mutate the body, wrap response
// streams, or observe traffic without knowing about each other.
package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/middleware"
)

// Config holds everything needed to start the proxy server.
type Config struct {
	Port        string
	UpstreamURL string
	Provider    string
}

// Server wraps the HTTP listener, the reverse proxy, and the middleware
// pipeline that runs on every request/response cycle.
type Server struct {
	config     Config
	proxy      *httputil.ReverseProxy
	httpServer *http.Server
	pipeline   *engine.Pipeline
}

// New builds a Server, wires the middleware pipeline, and returns a ready-
// to-start reverse proxy.
func New(cfg Config) (*Server, error) {
	if cfg.Port == "" {
		cfg.Port = "8080"
	}

	target, err := url.Parse(cfg.UpstreamURL)
	if err != nil {
		return nil, fmt.Errorf("proxy: invalid upstream URL %q: %w", cfg.UpstreamURL, err)
	}

	// --- middleware pipeline ----------------------------------------------
	pipeline := engine.New()

	// Adapter: host rewriting, request logging (no body mutation).
	pipeline.AddRequestHook(middleware.NewAdapter(target))

	// Compactor: inject extraction intent (on request) + SSE scanner + sync retry
	// (on response). Implements both RequestHook and ResponseHook.
	compactor := middleware.NewCompactor()
	pipeline.AddRequestHook(compactor)
	pipeline.AddResponseHook(compactor)

	// --- reverse proxy ---------------------------------------------------
	proxy := httputil.NewSingleHostReverseProxy(target)
	origDirector := proxy.Director

	// Context key for stashing request body between Director and ModifyResponse.
	type bodyCtxKey struct{}

	proxy.Director = func(req *http.Request) {
		var body []byte
		if req.Body != nil {
			body, _ = io.ReadAll(req.Body)
			req.Body.Close()
		}
		body = pipeline.RunBeforeRequest(req, body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))

		// Stash body so ModifyResponse can use it for sync retries.
		ctx := context.WithValue(req.Context(), bodyCtxKey{}, body)
		*req = *req.WithContext(ctx)

		origDirector(req)
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		body, _ := resp.Request.Context().Value(bodyCtxKey{}).([]byte)
		resp.Body = pipeline.RunAfterResponse(resp, resp.Body, resp.Request, body)
		return nil
	}

	// --- HTTP server -----------------------------------------------------
	mux := http.NewServeMux()
	mux.HandleFunc("/", proxy.ServeHTTP)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // disabled – SSE streams are long-lived
		IdleTimeout:  120 * time.Second,
	}

	return &Server{
		config:     cfg,
		proxy:      proxy,
		httpServer: srv,
		pipeline:   pipeline,
	}, nil
}

// --- Lifecycle --------------------------------------------------------------

func (s *Server) ListenAndServe() error {
	log.Printf("Torana Edge → %s  upstream: %s (%s)", s.httpServer.Addr, s.config.UpstreamURL, s.config.Provider)
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("proxy: listen error: %w", err)
	}
	return nil
}

func (s *Server) Serve(ln net.Listener) error {
	log.Printf("Torana Edge → %s  upstream: %s (%s)", ln.Addr(), s.config.UpstreamURL, s.config.Provider)
	if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("proxy: serve error: %w", err)
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}
