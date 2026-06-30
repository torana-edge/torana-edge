// Package proxy implements the Torana Edge reverse proxy engine.
//
// It sits between a developer agent harness (e.g., oh-my-pi) and cloud
// LLM providers. Requests arrive at /provider/<name>/<path> and are routed
// to the matching upstream. A modular middleware pipeline (internal/engine)
// intercepts every request/response pair.
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
	"strings"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/middleware"
	"github.com/torana-edge/torana-edge/internal/provider"
)

// Config holds everything needed to start the proxy server.
type Config struct {
	// Port is the TCP port the proxy listens on (e.g. "8080").
	Port string

	// Providers is the provider configuration (URLs, formats).
	Providers provider.Config

	// DefaultProvider routes requests without a /provider/<name>/ prefix
	// to this provider. Empty means no default — such requests get 502.
	DefaultProvider string
}

// Server wraps the HTTP listener, the reverse proxy, and the middleware
// pipeline that runs on every request/response cycle.
type Server struct {
	config     Config
	proxy      *httputil.ReverseProxy
	httpServer *http.Server
	pipeline   *engine.Pipeline
}

// --- Construction -----------------------------------------------------------

// New builds a Server and wires the middleware pipeline.
func New(cfg Config) (*Server, error) {
	if cfg.Port == "" {
		cfg.Port = "8080"
	}
	if cfg.Providers.Providers == nil {
		cfg.Providers.Providers = map[string]provider.Provider{}
	}

	// --- middleware pipeline ----------------------------------------------
	pipeline := engine.New()
	pipeline.AddRequestHook(middleware.NewAdapter())

	// --- reverse proxy ---------------------------------------------------
	// Context keys for stashing format and chat between Director and ModifyResponse.
	type formatCtxKey struct{}
	type chatCtxKey struct{}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			var body []byte
			if req.Body != nil {
				body, _ = io.ReadAll(req.Body)
				req.Body.Close()
			}

			prov, provName, strippedPath := provider.Resolve(req.URL.Path, cfg.Providers)

			// Try default provider fallback for non-prefixed paths.
			if prov == nil && cfg.DefaultProvider != "" {
				if dp, ok := cfg.Providers.Providers[cfg.DefaultProvider]; ok {
					prov = &dp
					provName = cfg.DefaultProvider
					strippedPath = req.URL.Path
				}
			}

			if prov == nil || len(body) == 0 {
				// Pass-through: no provider match, or empty body.
				_ = pipeline.RunBeforeRequest(req.Context(), req, nil)
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.ContentLength = int64(len(body))
				return
			}

			// Look up the format adapter.
			fmt := format.Lookup(prov.Format)

			// Rewrite the URL to point at the provider's upstream.
			target, err := url.Parse(prov.URL)
			if err != nil {
				log.Printf("provider %s: invalid URL %q: %v", provName, prov.URL, err)
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.ContentLength = int64(len(body))
				return
			}
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.URL.Path = joinURLPath(target.Path, strippedPath)
			req.URL.RawPath = ""

			if fmt == nil {
				// No format adapter for this provider's format — just forward raw.
				_ = pipeline.RunBeforeRequest(req.Context(), req, nil)
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.ContentLength = int64(len(body))
				return
			}

			chat, err := fmt.Request.Unmarshal(body)
			if err != nil {
				log.Printf("format %s unmarshal error: %v — passing through", fmt.Name, err)
				_ = pipeline.RunBeforeRequest(req.Context(), req, nil)
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.ContentLength = int64(len(body))
				return
			}

			chat = pipeline.RunBeforeRequest(req.Context(), req, chat)

			newBody, err := fmt.Request.Marshal(chat)
			if err != nil {
				log.Printf("format %s marshal error: %v — passing through", fmt.Name, err)
				newBody = body
			}

			// Stash format and chat for ModifyResponse.
			ctx := req.Context()
			ctx = context.WithValue(ctx, formatCtxKey{}, fmt)
			ctx = context.WithValue(ctx, chatCtxKey{}, chat)
			*req = *req.WithContext(ctx)

			req.Body = io.NopCloser(bytes.NewReader(newBody))
			req.ContentLength = int64(len(newBody))
		},

		ModifyResponse: func(resp *http.Response) error {
			if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
				return nil
			}

			fmt, _ := resp.Request.Context().Value(formatCtxKey{}).(*format.Format)
			if fmt == nil {
				return nil
			}

			chat, _ := resp.Request.Context().Value(chatCtxKey{}).(*engine.ChatRequest)

			events := fmt.Stream.ParseStream(resp.Body)
			events = pipeline.RunAfterResponse(resp.Request.Context(), resp, events, resp.Request, chat)

			pr, pw := io.Pipe()
			go func() {
				defer pw.Close()
				if err := fmt.Stream.SerializeStream(pw, events); err != nil {
					log.Printf("format %s serialize error: %v", fmt.Name, err)
				}
			}()
			resp.Body = pr
			return nil
		},
	}

	// --- HTTP server -----------------------------------------------------
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// If no provider matches and no default, reject.
		prov, _, _ := provider.Resolve(r.URL.Path, cfg.Providers)
		if prov == nil && cfg.DefaultProvider == "" {
			http.Error(w, "no provider configured for this path", http.StatusBadGateway)
			return
		}
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
		pipeline:   pipeline,
	}, nil
}

// --- Lifecycle --------------------------------------------------------------

func (s *Server) ListenAndServe() error {
	log.Printf("Torana Edge → :%s  providers: %d", s.config.Port, len(s.config.Providers.Providers))
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("proxy: listen error: %w", err)
	}
	return nil
}

func (s *Server) Serve(ln net.Listener) error {
	log.Printf("Torana Edge → %s  providers: %d", ln.Addr(), len(s.config.Providers.Providers))
	if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("proxy: serve error: %w", err)
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// joinURLPath concatenates a base path with a relative path, preserving
// double slashes where appropriate (mirrors httputil.singleJoiningSlash).
func joinURLPath(base, rel string) string {
	bs := strings.TrimSuffix(base, "/")
	rs := strings.TrimPrefix(rel, "/")
	if rs == "" {
		if bs == "" {
			return "/"
		}
		return bs
	}
	return bs + "/" + rs
}
