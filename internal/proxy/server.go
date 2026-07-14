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

const maxBodySize = 10 * 1024 * 1024 // 10 MB

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
	proxy      *httputil.ReverseProxy
	httpServer *http.Server
	stats      *metrics.StatsTracker
	// WASM plugin pipeline (loaded when configured)
	pluginPipeline atomic.Value // *plugin.PluginPipeline
	rateLimiter    *RateLimiter
}

type routeContextKey struct{}

type RouteContext struct {
	ProviderName string
	StrippedPath string
	Identity     string
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
		config:      cfg,
		stats:       statsTracker,
		rateLimiter: NewRateLimiter(cfg.Providers.Limits.RPM, cfg.Providers.Limits.Concurrency),
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
				lr := io.LimitReader(req.Body, maxBodySize+1)
				body, _ = io.ReadAll(lr)
				req.Body.Close()
				if len(body) > maxBodySize {
					log.Printf("request body exceeds max size after preflight limit")
					req.Body = io.NopCloser(bytes.NewReader(nil))
					req.ContentLength = 0
					return
				}
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

			if prov == nil {
				// Pass-through: no provider match.
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.ContentLength = int64(len(body))
				return
			}

			// Inject explicit routing metadata so the transport layer (failover)
			// doesn't have to guess from the mutated URL.
			ctx := context.WithValue(req.Context(), routeContextKey{}, &RouteContext{
				ProviderName: provName,
				StrippedPath: strippedPath,
			})
			*req = *req.WithContext(ctx)

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

			if fmt == nil || len(body) == 0 {
				// No format adapter, or empty body (e.g. GET /models). Just forward.
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

			// Inject headers into ToranaMeta for plugins (e.g. auth).
			if chat.ToranaMeta == nil {
				chat.ToranaMeta = make(map[string]any)
			}
			headers := make(map[string]any)
			for k, v := range req.Header {
				if len(v) > 0 {
					headers[k] = v[0]
				}
			}
			chat.ToranaMeta["_request_headers"] = headers

			// --- WASM plugin pipeline (runs before native hooks) ----------

			if pp := s.pluginPipeline.Load(); pp != nil {
				pl := pp.(*plugin.PluginPipeline)
				modified, err := pl.RunBeforeRequest(req.Context(), 0, chat)
				if err != nil {
					log.Printf("plugin pipeline error: %v", err)
				} else if modified != nil {
					chat = modified
				}
			}

			identity := ""
			if chat.ToranaMeta != nil {
				if id, ok := chat.ToranaMeta["identity"].(string); ok {
					identity = id
				}
			}
			if identity == "" {
				identity = req.Header.Get("Authorization")
			}
			rc := req.Context().Value(routeContextKey{}).(*RouteContext)
			rc.Identity = identity

			newBody, err := fmt.Request.Marshal(chat)
			if err != nil {
				log.Printf("format %s marshal error: %v — passing through", fmt.Name, err)
				newBody = body
			}

			// Stash format and chat for ModifyResponse.
			ctx = req.Context()
			ctx = context.WithValue(ctx, formatCtxKey{}, fmt)
			ctx = context.WithValue(ctx, chatCtxKey{}, chat)
			*req = *req.WithContext(ctx)

			req.Body = io.NopCloser(bytes.NewReader(newBody))
			req.ContentLength = int64(len(newBody))
			log.Printf("Proxying request to %s", req.URL.String())
		},

		ModifyResponse: func(resp *http.Response) error {
			// Skip pipeline for error responses — don't try to
			// reverse-translate a 4xx/5xx body that isn't a valid
			// chat completion response.
			log.Printf("Upstream returned %d", resp.StatusCode)
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

				events := fmt.Stream.ParseStream(resp.Body)

				// Hook WASM pipeline into the stream
				if pp := s.pluginPipeline.Load(); pp != nil {
					pl := pp.(*plugin.PluginPipeline)
					out := make(chan engine.StreamEvent)
					in := events
					go func() {
						defer close(out)
						for event := range in {
							// Call on_stream_chunk
							modified, err := pl.RunOnStreamChunk(resp.Request.Context(), 0, &event)
							if err != nil {
								log.Printf("plugin stream error: %v", err)
								out <- event
							} else if modified != nil {
								out <- *modified
								// Always forward the original ToolCallEnd after a plugin's
								// modification so the client receives the end-of-tool-call signal.
								if event.ToolCallEnd != nil {
									out <- event
								}
							} else {
								out <- event
							}
						}
					}()
					events = out
				}

				pr, pw := io.Pipe()
				go func() {
					defer pw.Close()
					if err := fmt.Stream.SerializeStream(pw, events); err != nil {
						log.Printf("format %s serialize error: %v", fmt.Name, err)
					}
				}()
				resp.Body = pr
				resp.Header.Del("Content-Length")
				return nil
			}

			// Non-streaming JSON:
			if strings.Contains(contentType, "application/json") {

				lr := io.LimitReader(resp.Body, maxBodySize+1)
				bodyBytes, err := io.ReadAll(lr)
				resp.Body.Close()
				if err != nil {
					return err
				}
				if len(bodyBytes) > maxBodySize {
					return fmt.Errorf("response body exceeds max size")
				}

				// Route JSON response through WASM response hooks.
				if pp := s.pluginPipeline.Load(); pp != nil {
					pl := pp.(*plugin.PluginPipeline)
					modified, modErr := runWASMOnJSONResponse(resp, pl, bodyBytes)
					if modErr != nil {
						log.Printf("wasm json response hook error: %v", modErr)
					} else {
						bodyBytes = modified
					}
				}
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

		// Enforce request body limit before it reaches Director or failover
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
			// Read the whole body now to trigger the 413 if it's too large
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "Request Entity Too Large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
		tr := &trackingReader{ReadCloser: r.Body}
		tw := &trackingWriter{ResponseWriter: w}
		r.Body = tr

		proxy.ServeHTTP(tw, r)

		s.stats.RecordRequest(tr.bytesRead, tw.bytesWritten)
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
		rateLimiter: s.rateLimiter,
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


// runWASMOnJSONResponse routes a non-streaming JSON response body through
// the WASM plugin pipeline. It parses tool calls from the JSON, creates
// synthetic StreamEvents, runs them through RunOnStreamChunk, collects
// modifications, and calls RunAfterResponse for post-processing.
func runWASMOnJSONResponse(resp *http.Response, pl *plugin.PluginPipeline, bodyBytes []byte) ([]byte, error) {
	// Parse the JSON response body to find tool calls.
	var parsed struct {
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content,omitempty"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls,omitempty"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return bodyBytes, nil // not our format, pass through
	}

	// Check if there are tool calls to process.
	hasToolCalls := false
	for _, c := range parsed.Choices {
		if len(c.Message.ToolCalls) > 0 {
			hasToolCalls = true
			break
		}
	}
	if !hasToolCalls {
		return bodyBytes, nil
	}

	ctx := resp.Request.Context()

	// Build synthetic StreamEvents and run through on_stream_chunk hooks.
	modified := false
	for ci := range parsed.Choices {
		for ti := range parsed.Choices[ci].Message.ToolCalls {
			tc := &parsed.Choices[ci].Message.ToolCalls[ti]

			// ToolCallStart
			startEv := engine.StreamEvent{
				ToolCallStart: &engine.ToolCallStart{
					Index: ti,
					ID:    tc.ID,
					Name:  tc.Function.Name,
				},
			}
			modStart, err := pl.RunOnStreamChunk(ctx, 0, &startEv)
			if err != nil {
				return bodyBytes, err
			}
			// Track name changes
			if modStart != nil && modStart.ToolCallStart != nil {
				tc.Function.Name = modStart.ToolCallStart.Name
			}

			// ToolCallDelta (full args)
			if tc.Function.Arguments != "" {
				deltaEv := engine.StreamEvent{
					ToolCallDelta: &engine.ToolCallDelta{
						Index:          ti,
						ArgumentsDelta: tc.Function.Arguments,
					},
				}
				modDelta, err := pl.RunOnStreamChunk(ctx, 0, &deltaEv)
				if err != nil {
					return bodyBytes, err
				}
				if modDelta != nil && modDelta.ToolCallDelta != nil && modDelta.ToolCallDelta.ArgumentsDelta != tc.Function.Arguments {
					tc.Function.Arguments = modDelta.ToolCallDelta.ArgumentsDelta
					modified = true
				}
			}

			// ToolCallEnd
			endEv := engine.StreamEvent{
				ToolCallEnd: &engine.ToolCallEnd{Index: ti},
			}
			if _, err := pl.RunOnStreamChunk(ctx, 0, &endEv); err != nil {
				return bodyBytes, err
			}
		}
	}

	if !modified {
		return bodyBytes, nil
	}

	out, err := json.Marshal(parsed)
	if err != nil {
		return bodyBytes, err
	}
	return out, nil
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

type trackingWriter struct {
	http.ResponseWriter
	bytesWritten int64
}

func (tw *trackingWriter) Write(b []byte) (int, error) {
	n, err := tw.ResponseWriter.Write(b)
	tw.bytesWritten += int64(n)
	return n, err
}

func (tw *trackingWriter) Flush() {
	if f, ok := tw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type trackingReader struct {
	io.ReadCloser
	bytesRead int64
}

func (tr *trackingReader) Read(p []byte) (n int, err error) {
	n, err = tr.ReadCloser.Read(p)
	tr.bytesRead += int64(n)
	return n, err
}
