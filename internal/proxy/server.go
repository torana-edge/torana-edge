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
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
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
	configMu   sync.RWMutex
	config     Config
	proxy       *httputil.ReverseProxy
	httpServer  *http.Server
	stats       *metrics.StatsTracker
	// WASM plugin pipeline (loaded when configured)
	pluginPipeline atomic.Value // *plugin.PluginPipeline
}

// --- Construction -----------------------------------------------------------

// New builds a Server and wires the middleware pipeline.
func New(cfg Config) (*Server, error) {
	log.Printf("DEBUG New: providers=%d plugins.dir=%q plugins.order=%v", len(cfg.Providers.Providers), cfg.Providers.Plugins.Dir, cfg.Providers.Plugins.Order)
	if cfg.Port == "" {
		cfg.Port = "8080"
	}
	if cfg.Providers.Providers == nil {
		cfg.Providers.Providers = map[string]provider.Provider{}
	}

	// --- middleware pipeline (now WASM) -----------------------------------
	statsTracker := metrics.NewStatsTracker()

	s := &Server{
		config:         cfg,
		stats:          statsTracker,
		
	}

	// --- WASM plugin pipeline (optional) ---------------------------------
	if cfg.Providers.Plugins.Dir != "" {
		rt := wasm.NewRuntime(context.Background())
		pp, err := plugin.NewPipeline(rt, plugin.PluginConfig{
			Dir:    cfg.Providers.Plugins.Dir,
			Order:  cfg.Providers.Plugins.Order,
			Config: cfg.Providers.Plugins.Config,
		})
		if err != nil {
			log.Printf("plugin pipeline: %v", err)
		} else {
			s.pluginPipeline.Store(pp)
			log.Printf("plugin pipeline: %d plugins loaded", len(cfg.Providers.Plugins.Order))
			go plugin.WatchPlugins(context.Background(), s.config.Providers.Plugins.Dir, plugin.PluginConfig{Dir: s.config.Providers.Plugins.Dir, Order: s.config.Providers.Plugins.Order, Config: s.config.Providers.Plugins.Config}, func(newPP *plugin.PluginPipeline) {
				old := s.pluginPipeline.Swap(newPP)
				if old != nil {
					go old.(*plugin.PluginPipeline).DrainAndClose()
				}
			})
		}
	}

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

			currentCfg := s.GetConfig()
			prov, provName, strippedPath := provider.Resolve(req.URL.Path, currentCfg.Providers)

			// Try default provider fallback for non-prefixed paths.
			if prov == nil && currentCfg.DefaultProvider != "" {
				if dp, ok := currentCfg.Providers.Providers[currentCfg.DefaultProvider]; ok {
					prov = &dp
					provName = currentCfg.DefaultProvider
					strippedPath = req.URL.Path
				}
			}

			if prov == nil || len(body) == 0 {
				// Pass-through: no provider match, or empty body.
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
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.ContentLength = int64(len(body))
				return
			}

			chat, err := fmt.Request.Unmarshal(body)
			if err != nil {
				log.Printf("format %s unmarshal error: %v — passing through", fmt.Name, err)
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.ContentLength = int64(len(body))
				return
			}

			// --- WASM plugin pipeline (runs before native hooks) ----------
			s.stats.RecordRequest(int64(len(body)), 0)

			if pp := s.pluginPipeline.Load(); pp != nil {
				pl := pp.(*plugin.PluginPipeline)
				chatJSON, _ := json.Marshal(chat)
				modifiedJSON, err := pl.RunOnChatRequest(req.Context(), chatJSON)
				if err != nil {
					log.Printf("plugin pipeline error: %v", err)
				} else if len(modifiedJSON) > 0 {
					var modified engine.ChatRequest
					if json.Unmarshal(modifiedJSON, &modified) == nil {
						chat = &modified
					}
				}
			}

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
			// Skip pipeline for error responses — don't try to
			// reverse-translate a 4xx/5xx body that isn't a valid
			// chat completion response.
			if resp.StatusCode >= 400 {
				return nil
			}

			contentType := resp.Header.Get("Content-Type")

			// SSE streaming: parse → pipeline → serialize.
			if strings.Contains(contentType, "text/event-stream") {
				fmt, _ := resp.Request.Context().Value(formatCtxKey{}).(*format.Format)
				if fmt == nil {
					return nil
				}

				// WASM response hooks not yet implemented for SSE
				events := fmt.Stream.ParseStream(resp.Body)

				pr, pw := io.Pipe()
				go func() {
					defer pw.Close()
					if err := fmt.Stream.SerializeStream(pw, events); err != nil {
						log.Printf("format %s serialize error: %v", fmt.Name, err)
					}
				}()
				resp.Body = pr
				return nil
			}

			// Non-streaming JSON:
			if strings.Contains(contentType, "application/json") {

				bodyBytes, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					return nil
				}

				// WASM response hooks not yet implemented for JSON
				processed := bodyBytes

				resp.Body = io.NopCloser(bytes.NewReader(processed))
				resp.ContentLength = int64(len(processed))
				return nil
			}

			return nil
		},
	}

	// --- HTTP server -----------------------------------------------------
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(s.stats)
		w.Write(b)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		currentCfg := s.GetConfig()
		// Panic recovery for the request handler goroutine.
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic in request handler: %v", rec)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		// If no provider matches and no default, reject.
		prov, _, _ := provider.Resolve(r.URL.Path, currentCfg.Providers)
		if prov == nil && currentCfg.DefaultProvider == "" {
			http.Error(w, "no provider configured for this path", http.StatusBadGateway)
			return
		}
		proxy.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // disabled – SSE streams are long-lived
		IdleTimeout:  120 * time.Second,
	}

	// Wire failover transport so the proxy retries across fallback providers.
	proxy.Transport = &failoverRoundTripper{
		base: http.DefaultTransport,
		cfg: func() provider.Config {
			return s.GetConfig().Providers
		},
	}

	s.proxy = proxy
	s.httpServer = srv
	return s, nil
}

// --- Lifecycle --------------------------------------------------------------

func (s *Server) GetConfig() Config {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.config
}

// SetProviders hot-reloads the provider configuration without restarting.
func (s *Server) SetProviders(cfg provider.Config) {
	s.configMu.Lock()
	s.config.Providers = cfg
	s.configMu.Unlock()
	log.Printf("config hot-reload: %d providers loaded", len(cfg.Providers))
}

func (s *Server) ListenAndServe() error {
	cfg := s.GetConfig()
	log.Printf("Torana Edge → :%s  providers: %d", cfg.Port, len(cfg.Providers.Providers))
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("proxy: listen error: %w", err)
	}
	return nil
}

func (s *Server) Serve(ln net.Listener) error {
	cfg := s.GetConfig()
	log.Printf("Torana Edge → %s  providers: %d", ln.Addr(), len(cfg.Providers.Providers))
	if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("proxy: serve error: %w", err)
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	if pp := s.pluginPipeline.Load(); pp != nil {
		pp.(*plugin.PluginPipeline).DrainAndClose()
	}
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
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


