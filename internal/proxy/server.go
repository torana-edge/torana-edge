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
	"time"

	"github.com/torana-edge/torana-edge/internal/cache"
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
	configMu   sync.RWMutex
	config     Config
	proxy       *httputil.ReverseProxy
	httpServer  *http.Server
	pipeline    *engine.Pipeline
	intentCache cache.IntentCache
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
	intentCache := cache.NewLocalCache(30 * time.Minute)
	compactionCache := cache.NewLocalCache(10 * time.Minute)
	translator := middleware.NewSchemaTranslator(intentCache)
	pipeline.AddRequestHook(translator)
	pipeline.AddResponseHook(translator)

	// Offload hook — compacts tool results using a cheaper model.
	// Runs after schema translator so intent cache is populated.
	offloadCfg := cfg.Providers.Offload
	if offloadCfg.Enabled {
		offloadProvider := offloadCfg.Provider
		if offloadProvider == "" {
			offloadProvider = "deepseek"
		}
		offloadURL := ""
		if p, ok := cfg.Providers.Providers[offloadProvider]; ok {
			offloadURL = p.URL
		}
		offloadHook := middleware.NewOffloadHook(translator.IntentCache, compactionCache, offloadCfg, offloadURL)
		pipeline.AddRequestHook(offloadHook)
		log.Printf("Torana Edge → offload enabled: model=%s provider=%s url=%s",
			offloadCfg.Model, offloadProvider, offloadURL)
	}

	s := &Server{
		config:      cfg,
		pipeline:    pipeline,
		intentCache: intentCache,
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
			}

			// Non-streaming JSON: convert to synthetic StreamEvents,
			// run through response pipeline, rebuild JSON.
			if strings.Contains(contentType, "application/json") {
				chat, _ := resp.Request.Context().Value(chatCtxKey{}).(*engine.ChatRequest)
				if chat == nil {
					return nil
				}

				bodyBytes, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					return nil
				}

				processed, err := runNonStreamingPipeline(resp, pipeline, chat, bodyBytes)
				if err != nil {
					log.Printf("non-streaming pipeline error: %v", err)
					processed = bodyBytes // passthrough on error
				}

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
	if s.intentCache != nil {
		s.intentCache.Close()
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

// runNonStreamingPipeline handles non-streaming (application/json) LLM
// responses by converting tool calls into synthetic StreamEvents, running
// them through the response pipeline, and rebuilding the JSON body from
// the processed events. This ensures all ResponseHooks fire for
// non-streaming responses just like they do for SSE.
func runNonStreamingPipeline(resp *http.Response, pipeline *engine.Pipeline, chat *engine.ChatRequest, body []byte) ([]byte, error) {
	// Parse the OpenAI Chat Completions response shape.
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
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body, nil // not our format, pass through
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
		return body, nil // nothing to process
	}

	// Build synthetic StreamEvents: ToolCallStart → ToolCallDelta(full args) → ToolCallEnd
	// for each tool call. Use a buffered channel and close it before running pipeline.
	events := make(chan engine.StreamEvent, len(parsed.Choices)*3)
	for _, c := range parsed.Choices {
		for i, tc := range c.Message.ToolCalls {
			events <- engine.StreamEvent{
				ToolCallStart: &engine.ToolCallStart{
					Index: i,
					ID:    tc.ID,
					Name:  tc.Function.Name,
				},
			}
			if tc.Function.Arguments != "" {
				args := tc.Function.Arguments
				events <- engine.StreamEvent{
					ToolCallDelta: &engine.ToolCallDelta{
						Index:          i,
						ArgumentsDelta: args,
					},
				}
			}
			events <- engine.StreamEvent{
				ToolCallEnd: &engine.ToolCallEnd{Index: i},
			}
		}
	}
	close(events)

	// Run through the response pipeline.
	processed := pipeline.RunAfterResponse(resp.Request.Context(), resp, events, resp.Request, chat)

	// Collect processed events. We only care about ToolCallDeltas
	// (which contain the sanitized arguments).
	type tcResult struct {
		args string
	}
	results := make(map[int]*tcResult) // index → result

	for ev := range processed {
		if ev.ToolCallDelta != nil {
			if r, ok := results[ev.ToolCallDelta.Index]; ok {
				r.args += ev.ToolCallDelta.ArgumentsDelta
			} else {
				results[ev.ToolCallDelta.Index] = &tcResult{
					args: ev.ToolCallDelta.ArgumentsDelta,
				}
			}
		}
	}

	// Rebuild the JSON with sanitized arguments.
	modified := false
	for i := range parsed.Choices {
		for j := range parsed.Choices[i].Message.ToolCalls {
			tc := &parsed.Choices[i].Message.ToolCalls[j]
			if r, ok := results[j]; ok && r.args != tc.Function.Arguments {
				tc.Function.Arguments = r.args
				modified = true
			}
		}
	}

	if !modified {
		return body, nil
	}

	out, err := json.Marshal(parsed)
	if err != nil {
		return body, err
	}
	return out, nil
}
