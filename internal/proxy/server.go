// Package proxy implements the Torana Edge reverse proxy engine.
//
// It sits between a developer agent harness (e.g., oh-my-pi) and cloud
// LLM providers. Requests arrive at /provider/<name>/<path> and are routed
// to the matching upstream. A WASM plugin pipeline (internal/plugin +
// internal/wasm) intercepts every request/response pair.
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
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

const maxBodySize = 10 * 1024 * 1024 // 10 MB

// allowedPluginHeaders is the only set of request headers ever exposed to
// plugins (via ToranaMeta["_request_headers"]), and only when a loaded
// plugin holds the env.request_headers permission.
var allowedPluginHeaders = []string{
	"Authorization",
	"X-Api-Key",
	"X-Torana-User",
	"X-Torana-Team",
	"X-Torana-Tenant",
}

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

// Server wraps the HTTP listener, the reverse proxy, and the WASM plugin
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
	// watchCancel stops the plugin hot-reload watcher on Shutdown.
	watchCancel context.CancelFunc
}

type routeContextKey struct{}

type RouteContext struct {
	ProviderName string
	StrippedPath string
	Identity     string
	// Block, when set by the Director after a plugin vetoes the request
	// (env.block_request), tells the transport to return this synthetic
	// error response instead of calling upstream.
	Block *BlockResponse
}

// BlockResponse is a synthetic, provider-shaped error a plugin requested via
// env.block_request. The transport returns it verbatim; no upstream call is made.
type BlockResponse struct {
	Status      int
	ContentType string
	Body        []byte
}

// reqCounter issues unique request IDs used to scope plugin state.
var reqCounter atomic.Uint64

type reqStateKey struct{}

// reqState carries per-request data from the HTTP handler into the
// Director, ModifyResponse, and WASM host calls (via context).
type reqState struct {
	ID uint64
	// CallerAuth is the caller's Authorization header value, used as the
	// fallback credential for offload completions. Host-side only — never
	// exposed to plugins.
	CallerAuth string
	// Pipeline is the plugin pipeline pinned for this request's entire
	// lifetime (Acquire held until the handler's deferred Release). Every
	// phase — Director, stream hooks, ModifyResponse, EndRequest — MUST
	// use this instead of re-loading s.pluginPipeline: a hot-reload swap
	// mid-request would otherwise drain and close the runtime holding this
	// request's meta state (fragment buffers, mutation registry).
	Pipeline *plugin.PluginPipeline

	// Observability fields, populated across phases and read by the handler
	// after ServeHTTP to emit host request metrics. Model/Provider are set in
	// the Director; UpstreamStatus in ModifyResponse.
	Model          string
	Provider       string
	UpstreamStatus int
	// Start marks when the handler began proxying (drives _response.duration_ms
	// and the host latency metric).
	Start time.Time
	// UsageIn/UsageOut are provider-reported token counts, captured from the
	// stream usage frame or the JSON usage object. Zero when the provider
	// didn't report.
	UsageIn  int
	UsageOut int
	// UsageInjected marks that the Director opted an openai stream into
	// stream_options.include_usage on the caller's behalf; the resulting
	// usage frame is consumed host-side and never forwarded to the client.
	UsageInjected bool
	// Synthetic marks a response served by a plugin (env.respond_request):
	// the transport returns it verbatim and ModifyResponse must not re-parse
	// it or run response hooks over it.
	Synthetic bool
	// OriginalReq is the pristine pre-pipeline request (pb bytes), snapshotted
	// only when a loaded plugin holds env.original_request.
	OriginalReq []byte
	// OriginalResp is the raw upstream response body (non-streaming JSON path
	// only), stashed before response hooks run, only when a loaded plugin
	// holds env.original_response.
	OriginalResp []byte
}

// responseMeta builds the _response signal handed to run_after_response so
// plugins can observe latency, upstream status, and token usage.
func (rs *reqState) responseMeta() map[string]any {
	durationMs := 0.0
	if !rs.Start.IsZero() {
		durationMs = float64(time.Since(rs.Start).Microseconds()) / 1000
	}
	return map[string]any{
		"duration_ms":     durationMs,
		"upstream_status": rs.UpstreamStatus,
		"usage": map[string]any{
			"input_tokens":  rs.UsageIn,
			"output_tokens": rs.UsageOut,
		},
	}
}

// reqStateFrom returns the request state stashed by the HTTP handler,
// or a zero-value fallback for requests outside the handler (tests).
func reqStateFrom(ctx context.Context) *reqState {
	if rs, ok := ctx.Value(reqStateKey{}).(*reqState); ok {
		return rs
	}
	return &reqState{}
}

// --- Construction -----------------------------------------------------------

// New builds a Server and wires the WASM plugin pipeline.
func New(cfg Config) (*Server, error) {
	if cfg.Port == "" {
		cfg.Port = "8080"
	}
	if cfg.Providers.Providers == nil {
		cfg.Providers.Providers = map[string]provider.Provider{}
	}

	// --- stats tracker -----------------------------------------------------
	statsTracker := metrics.NewStatsTracker()
	// Bridge cumulative savings/throughput counters to OTLP (no-op if OTel
	// is disabled; InitOTel runs before New in main).
	metrics.RegisterStatsObservables(statsTracker)

	s := &Server{
		config:      cfg,
		stats:       statsTracker,
		rateLimiter: NewRateLimiter(cfg.Providers.Limits.RPM, cfg.Providers.Limits.Concurrency),
	}

	// --- offload validation (fail fast on misconfiguration) ---------------
	if err := cfg.Providers.Offload.Validate(cfg.Providers.Providers); err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	if off := cfg.Providers.Offload; off.Enabled {
		switch {
		case off.APIKeyEnv == "":
			log.Printf("warning: offload enabled without offload.api_key_env — offload will reuse each caller's credential, which only authenticates when the offload provider %q shares the caller's auth. Set offload.api_key_env for cross-provider or local-model offload (e.g. a Claude/OpenAI caller summarizing on DeepSeek or a self-hosted SLM).", off.Provider)
		case os.Getenv(off.APIKeyEnv) == "":
			log.Printf("warning: offload.api_key_env %q is set but the env var is empty — falling back to caller credentials", off.APIKeyEnv)
		}
	}

	// --- WASM plugin pipeline (optional) ---------------------------------
	if cfg.Providers.Plugins.Dir != "" {
		// newRuntime wires host callbacks; used at startup AND on every
		// hot-reload — a bare runtime would silently lose offload/stats.
		newRuntime := func() *wasm.Runtime {
			rt := wasm.NewRuntime(context.Background())
			// Offload completion handler (cheap-model tool result
			// summarization), recording failures in /stats.
			rt.OffloadFunc = func(ctx context.Context, payloadJSON string) (string, error) {
				out, err := s.offloadCompletion(ctx, payloadJSON)
				if err != nil {
					// Plugins degrade gracefully on offload errors, so this
					// log line is the only host-side visibility.
					log.Printf("[offload] %v", err)
					s.stats.RecordOffloadFailure()
				}
				return out, err
			}
			// Plugins report compaction savings via torana_record_savings,
			// attributed per plugin in /stats and OTLP.
			rt.SavingsFunc = func(pluginName string, originalBytes, finalBytes int64) {
				s.stats.RecordCompaction(pluginName, originalBytes, finalBytes)
				metrics.RecordPluginSavings(context.Background(), pluginName, originalBytes-finalBytes)
			}
			// Pristine request/response snapshots (env.original_request /
			// env.original_response), read from the request state the same
			// way offload does.
			rt.OriginalRequestFunc = func(ctx context.Context) []byte {
				return reqStateFrom(ctx).OriginalReq
			}
			rt.OriginalResponseFunc = func(ctx context.Context) []byte {
				return reqStateFrom(ctx).OriginalResp
			}
			return rt
		}
		pp, err := plugin.NewPipeline(newRuntime(), plugin.PluginConfig{
			Dir:    cfg.Providers.Plugins.Dir,
			Order:  cfg.Providers.Plugins.Order,
			Config: cfg.Providers.Plugins.Config,
		})
		if err != nil {
			log.Printf("plugin pipeline: %v", err)
		} else {
			s.pluginPipeline.Store(pp)
			log.Printf("plugin pipeline: %d plugins loaded", pp.Len())
			watchCtx, watchCancel := context.WithCancel(context.Background())
			s.watchCancel = watchCancel
			// configFn reads the live config so plugin-config hot-reloads
			// apply on the next plugin reload.
			configFn := func() plugin.PluginConfig {
				p := s.GetConfig().Providers.Plugins
				return plugin.PluginConfig{Dir: p.Dir, Order: p.Order, Config: p.Config}
			}
			go plugin.WatchPlugins(watchCtx, cfg.Providers.Plugins.Dir, configFn, newRuntime, func(newPP *plugin.PluginPipeline) {
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

			if chat.ToranaMeta == nil {
				chat.ToranaMeta = make(map[string]any)
			}

			// --- WASM plugin pipeline --------------------------------------

			if pl := reqStateFrom(req.Context()).Pipeline; pl != nil {

				// Pristine-request snapshot for env.original_request, taken
				// BEFORE any meta injection or plugin mutation. Plugins are
				// chained (each sees its predecessor's output); this host call
				// is the only way to see what the caller actually sent.
				if pl.HasGrant("env.original_request") {
					if b, err := proto.Marshal(pbconv.ToPBChatRequest(chat)); err == nil {
						reqStateFrom(req.Context()).OriginalReq = b
					}
				}

				// Credential-bearing headers are exposed to plugins only
				// when a loaded plugin declares the env.request_headers
				// permission, and only from an allowlist — plugins must
				// never see arbitrary caller headers by default.
				if pl.HasGrant("env.request_headers") {
					headers := make(map[string]any)
					for _, k := range allowedPluginHeaders {
						if v := req.Header.Get(k); v != "" {
							headers[k] = v
						}
					}
					chat.ToranaMeta["_request_headers"] = headers
				}

				modified, err := pl.RunBeforeRequest(req.Context(), reqStateFrom(req.Context()).ID, chat)
				if err != nil {
					log.Printf("plugin pipeline error: %v", err)
				} else if modified != nil {
					chat = modified
				}
				// Defense in depth: never let credentials linger in meta
				// past the request hook (format adapters don't serialize
				// ToranaMeta, but response hooks receive it).
				if chat.ToranaMeta != nil {
					delete(chat.ToranaMeta, "_request_headers")
				}

				// Request veto: a plugin holding env.block_request may reject
				// the request outright. Honor it only when the capability is
				// declared, render a provider-shaped error, and short-circuit
				// — the transport returns rc.Block instead of calling upstream.
				if pl.HasGrant("env.block_request") && chat.ToranaMeta != nil {
					if raw, ok := chat.ToranaMeta["_block"]; ok {
						delete(chat.ToranaMeta, "_block")
						if rc, ok := req.Context().Value(routeContextKey{}).(*RouteContext); ok {
							rc.Block = renderBlock(prov.Format, raw)
						}
						req.Body = io.NopCloser(bytes.NewReader(nil))
						req.ContentLength = 0
						return
					}
				}

				// Respond-directly: a plugin holding env.respond_request may
				// serve the full response itself (response cache, mock mode).
				// The host renders a provider-shaped completion — SSE if the
				// client streams — and the transport returns it without
				// calling upstream: zero tokens spent. A block verdict wins
				// over a respond verdict (checked above).
				if pl.HasGrant("env.respond_request") && chat.ToranaMeta != nil {
					if raw, ok := chat.ToranaMeta["_respond"]; ok {
						delete(chat.ToranaMeta, "_respond")
						if rc, ok := req.Context().Value(routeContextKey{}).(*RouteContext); ok {
							rc.Block = renderRespond(fmt, chat.Model, raw, chat.Stream)
						}
						rs := reqStateFrom(req.Context())
						rs.Synthetic = true
						rs.Model = chat.Model
						rs.Provider = provName
						req.Body = io.NopCloser(bytes.NewReader(nil))
						req.ContentLength = 0
						return
					}
				}
			}

			// Record routing facts for host request metrics (read by the
			// handler after ServeHTTP).
			if rs := reqStateFrom(req.Context()); rs != nil {
				rs.Model = chat.Model
				rs.Provider = provName
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

			// Token usage on openai streams is opt-in; opt in on the caller's
			// behalf so the host can meter tokens. The resulting usage frame
			// is consumed host-side and suppressed from the client's stream
			// (see the usage tap in ModifyResponse) — unless the client asked
			// for it itself, in which case nothing is injected or suppressed.
			if fmt.Name == "openai" && chat.Stream {
				if _, ok := chat.ProviderExtensions["stream_options"]; !ok {
					if chat.ProviderExtensions == nil {
						chat.ProviderExtensions = map[string]any{}
					}
					chat.ProviderExtensions["stream_options"] = map[string]any{"include_usage": true}
					reqStateFrom(req.Context()).UsageInjected = true
				}
			}

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
			if rs := reqStateFrom(resp.Request.Context()); rs != nil {
				rs.UpstreamStatus = resp.StatusCode
				// Plugin-served response (env.respond_request): already a
				// complete, provider-shaped body — don't re-parse it or run
				// response hooks over it.
				if rs.Synthetic {
					return nil
				}
			}
			// Skip the mutation pipeline for error responses — don't try to
			// reverse-translate a 4xx/5xx body that isn't a valid chat
			// completion response. Audit/metrics plugins still observe the
			// outcome through an observe-only hook carrying _response.
			log.Printf("Upstream returned %d", resp.StatusCode)
			if resp.StatusCode >= 400 {
				ctx := resp.Request.Context()
				rs := reqStateFrom(ctx)
				if pl := rs.Pipeline; pl != nil {
					errChat := &engine.ChatRequest{
						Model:      rs.Model,
						ToranaMeta: map[string]any{"_response": rs.responseMeta()},
					}
					if _, err := pl.RunAfterResponse(ctx, rs.ID, errChat); err != nil {
						log.Printf("plugin run_after_response (error path): %v", err)
					}
				}
				return nil
			}

			contentType := resp.Header.Get("Content-Type")

			// SSE streaming: parse → pipeline → serialize.
			if strings.Contains(contentType, "text/event-stream") {
				fmt, _ := resp.Request.Context().Value(formatCtxKey{}).(*format.Format)
				if fmt == nil {
					return nil
				}

				// resp.Body is replaced with the serializer pipe below, so
				// nothing downstream ever closes the ORIGINAL upstream body
				// — which carries the rate-limiter release in its Close.
				// Close it explicitly when serialization finishes, or every
				// streamed request leaks a concurrency token.
				upstreamBody := resp.Body

				events := fmt.Stream.ParseStream(upstreamBody)

				// Host usage tap: record provider-reported tokens for metrics
				// and the _response signal. When the host injected the usage
				// opt-in (openai), the frame is dropped here so the client's
				// stream shape is exactly what it asked for; otherwise it
				// passes through (and on to plugins) untouched.
				rs := reqStateFrom(resp.Request.Context())
				{
					in := events
					tapped := make(chan engine.StreamEvent)
					go func() {
						defer close(tapped)
						for ev := range in {
							if ev.Usage != nil {
								rs.UsageIn, rs.UsageOut = ev.Usage.InputTokens, ev.Usage.OutputTokens
								if rs.UsageInjected {
									continue
								}
							}
							tapped <- ev
						}
					}()
					events = tapped
				}

				// Hook WASM pipeline into the stream. Plugins may suppress,
				// replace, or fan out each event (e.g. buffer argument
				// fragments and emit one complete ToolCallDelta before
				// ToolCallEnd). Uses the request-pinned pipeline — never
				// re-load s.pluginPipeline mid-request.
				if pl := reqStateFrom(resp.Request.Context()).Pipeline; pl != nil {
					reqID := reqStateFrom(resp.Request.Context()).ID
					out := make(chan engine.StreamEvent)
					in := events
					go func() {
						defer close(out)
						for event := range in {
							outEvents, err := pl.RunOnStreamChunk(resp.Request.Context(), reqID, &event)
							if err != nil {
								log.Printf("plugin stream error: %v", err)
								out <- event
								continue
							}
							for _, ev := range outEvents {
								out <- ev
							}
						}
					}()
					events = out
				}

				pr, pw := io.Pipe()
				go func() {
					defer pw.Close()
					defer upstreamBody.Close()
					if err := fmt.Stream.SerializeStream(pw, events); err != nil {
						log.Printf("format %s serialize error: %v", fmt.Name, err)
					}
					// Observational run_after_response for streaming
					// responses (metrics/audit plugins). Mutations are not
					// applied — the stream has already been written. The
					// _response signal (latency/status/usage) is complete
					// here: the whole stream has been serialized.
					ctx := resp.Request.Context()
					if pl := reqStateFrom(ctx).Pipeline; pl != nil {
						if chat, _ := ctx.Value(chatCtxKey{}).(*engine.ChatRequest); chat != nil {
							if chat.ToranaMeta == nil {
								chat.ToranaMeta = map[string]any{}
							}
							chat.ToranaMeta["_response"] = rs.responseMeta()
							if _, err := pl.RunAfterResponse(ctx, reqStateFrom(ctx).ID, chat); err != nil {
								log.Printf("plugin run_after_response (stream): %v", err)
							}
						}
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

				// Route the JSON response through the WASM response hooks
				// (run_on_stream_chunk over synthetic events, then
				// run_after_response) for every provider format. Uses the
				// request-pinned pipeline.
				ctx := resp.Request.Context()
				rs := reqStateFrom(ctx)
				f, _ := ctx.Value(formatCtxKey{}).(*format.Format)
				if pl := rs.Pipeline; pl != nil && f != nil {
					// Raw-body snapshot for env.original_response, before any
					// hook mutates it.
					if pl.HasGrant("env.original_response") {
						rs.OriginalResp = bodyBytes
					}
					chat, _ := ctx.Value(chatCtxKey{}).(*engine.ChatRequest)
					// Records provider usage into rs as a side effect.
					modified, modErr := runJSONResponseHooks(ctx, pl, rs.ID, f.Name, chat, bodyBytes)
					if modErr != nil {
						log.Printf("wasm json response hook error: %v", modErr)
					} else {
						bodyBytes = modified
					}
				} else if f != nil {
					// No pipeline — still meter provider-reported usage.
					var body map[string]any
					if json.Unmarshal(bodyBytes, &body) == nil {
						if u := extractResponse(f.Name, body).usage; u != nil {
							rs.UsageIn, rs.UsageOut = u.InputTokens, u.OutputTokens
						}
					}
				}

				resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				resp.ContentLength = int64(len(bodyBytes))
				// ReverseProxy copies resp.Header verbatim — a stale
				// Content-Length after a body-mutating hook makes the server
				// write more bytes than declared and abort the connection.
				resp.Header.Set("Content-Length", strconv.Itoa(len(bodyBytes)))
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
				// http.ErrAbortHandler is the sanctioned abort for client
				// disconnects (ReverseProxy panics with it by design) —
				// re-panic so net/http handles it quietly instead of
				// logging it as a crash.
				if err, ok := rec.(error); ok && err == http.ErrAbortHandler {
					panic(rec)
				}
				log.Printf("panic in request handler: %v", rec)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()

		// Assign a request ID scoping all plugin meta state, and stash
		// per-request data for the Director/ModifyResponse/offload path.
		// The pipeline is pinned (Acquire) for the whole request so a
		// hot-reload swap cannot drain-and-close the runtime that holds
		// this request's state mid-flight.
		rs := &reqState{
			ID:         reqCounter.Add(1),
			CallerAuth: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
			Start:      time.Now(),
		}
		if pp := s.pluginPipeline.Load(); pp != nil {
			rs.Pipeline = pp.(*plugin.PluginPipeline)
			rs.Pipeline.Acquire()
		}
		r = r.WithContext(context.WithValue(r.Context(), reqStateKey{}, rs))
		// Drop request-scoped plugin state when the request completes,
		// then release the pinned pipeline. Safe to defer here:
		// ReverseProxy.ServeHTTP only returns after the response body
		// (including the SSE pipe) is fully copied.
		defer func() {
			if rs.Pipeline != nil {
				rs.Pipeline.EndRequest(rs.ID)
				rs.Pipeline.Release()
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
		s.stats.RecordTokens(int64(rs.UsageIn), int64(rs.UsageOut))
		// Host request metrics: latency + outcome, labeled by model/provider.
		// The host sees every response (including errors and vetoes), so this
		// is the reliable source of truth for latency and status.
		metrics.RecordProxyRequest(r.Context(), rs.Model, rs.Provider, tw.status, float64(time.Since(rs.Start).Microseconds())/1000)
		metrics.RecordTokens(r.Context(), rs.Model, rs.Provider, rs.UsageIn, rs.UsageOut)
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
	if s.watchCancel != nil {
		s.watchCancel()
	}
	s.rateLimiter.Close()
	if pp := s.pluginPipeline.Load(); pp != nil {
		pp.(*plugin.PluginPipeline).DrainAndClose()
	}
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

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
	status       int
}

func (tw *trackingWriter) WriteHeader(code int) {
	tw.status = code
	tw.ResponseWriter.WriteHeader(code)
}

func (tw *trackingWriter) Write(b []byte) (int, error) {
	if tw.status == 0 {
		tw.status = http.StatusOK // implicit 200 on first write
	}
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
