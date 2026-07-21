// Package proxy implements the Torana Edge reverse proxy engine.
//
// It sits between a developer agent harness (e.g., oh-my-pi) and cloud
// LLM providers. Requests arrive at /provider/<name>/<path> and are routed
// to the matching upstream. A WASM plugin pipeline (internal/plugin +
// internal/wasm) intercepts every request/response pair.
package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/torana-edge/torana-edge/internal/cache"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
	"github.com/torana-edge/torana-edge/pkg/pb"
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

	// ConfigPath is the path to the config file on disk for persistence.
	ConfigPath string
}

// Server wraps the HTTP listener, the reverse proxy, and the WASM plugin
// pipeline that runs on every request/response cycle.
type Server struct {
	configMu   sync.RWMutex
	rebuildMu  sync.Mutex
	configPath string
	config     Config
	proxy      *httputil.ReverseProxy
	httpServer *http.Server
	stats      *metrics.StatsTracker
	// feed is the bounded in-memory ring buffer of recent per-request events,
	// exposed via /_torana/api/feed (snapshot) and /_torana/api/stream (SSE).
	feed *metrics.RequestFeed
	// WASM plugin pipeline (loaded when configured)
	pluginPipeline atomic.Value // *plugin.PluginPipeline
	// sharedCache is the cross-request plugin state store shared by every
	// runtime this server builds (survives hot-reloads; redis backend
	// survives restarts). Closed on Shutdown, after the pipeline drains.
	sharedCache cache.Store
	rateLimiter *RateLimiter
	// watchCancel stops the plugin hot-reload watcher on Shutdown.
	watchCancel context.CancelFunc
	watchDone   <-chan struct{}
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
	// UsageCacheRead/UsageCacheWrite are the provider-reported prompt-cache
	// token counts (read = served from cache, write = written to cache).
	// Zero when the provider didn't report or nothing was cached.
	UsageCacheRead  int
	UsageCacheWrite int
	// UsageInjected marks that the Director opted an openai stream into
	// stream_options.include_usage on the caller's behalf; the resulting
	// usage frame is consumed host-side and never forwarded to the client.
	UsageInjected bool
	// Synthetic marks a response served by a plugin (env.respond_request):
	// the transport returns it verbatim and ModifyResponse must not re-parse
	// it or run response hooks over it.
	Synthetic bool
	// Verdict is the control-plane outcome applied by the plugin pipeline:
	// "block" (env.block_request), "respond" (env.respond_request),
	// "route" (env.route_request). Empty when no pipeline is loaded or no
	// veto/redirect was applied.
	Verdict string
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
			"input_tokens":       rs.UsageIn,
			"output_tokens":      rs.UsageOut,
			"cache_read_tokens":  rs.UsageCacheRead,
			"cache_write_tokens": rs.UsageCacheWrite,
		},
	}
}

// mergeUsage folds a usage frame into the request state without zeroing
// counts a previous frame already reported (Anthropic splits input and output
// usage across message_start and message_delta).
func (rs *reqState) mergeUsage(u *engine.StreamUsage) {
	if u.InputTokens > 0 {
		rs.UsageIn = u.InputTokens
	}
	if u.OutputTokens > 0 {
		rs.UsageOut = u.OutputTokens
	}
	if u.CacheReadTokens > 0 {
		rs.UsageCacheRead = u.CacheReadTokens
	}
	if u.CacheWriteTokens > 0 {
		rs.UsageCacheWrite = u.CacheWriteTokens
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

	configPath := cfg.ConfigPath
	if configPath == "" {
		configPath = "config.json"
	}
	s := &Server{
		config:      cfg,
		configPath:  configPath,
		stats:       statsTracker,
		feed:        metrics.NewRequestFeed(0), // default 200-event ring buffer
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
		// One shared cross-request cache store for every runtime this server
		// ever builds: plugin state (compacted results, PII verdicts) must
		// survive hot-reload swaps, and the redis backend additionally makes
		// it survive restarts / span instances. Fail fast on a bad backend —
		// a deployment that asked for distributed state must not silently
		// fall back to per-process memory.
		sharedCache, err := cache.New(cfg.Providers.Cache)
		if err != nil {
			return nil, fmt.Errorf("proxy: %w", err)
		}
		s.sharedCache = sharedCache

		if err := s.RebuildPipeline(cfg.Providers.Plugins); err != nil {
			log.Printf("plugin pipeline: %v", err)
		} else {
			raw := s.pluginPipeline.Load()
			if raw != nil {
				pp := raw.(*plugin.PluginPipeline)
				log.Printf("plugin pipeline: %d plugins loaded", pp.Len())
			}
			watchCtx, watchCancel := context.WithCancel(context.Background())
			s.watchCancel = watchCancel
			// configFn reads the live config so plugin-config hot-reloads
			// apply on the next plugin reload.
			configFn := func() plugin.PluginConfig {
				p := s.GetConfig().Providers.Plugins
				return plugin.PluginConfig{Dir: p.Dir, Order: p.Order, Config: p.Config}
			}
			watchDone := make(chan struct{})
			s.watchDone = watchDone
			if err := plugin.WatchPlugins(watchCtx, cfg.Providers.Plugins.Dir, configFn, s.newRuntime, func(newPP *plugin.PluginPipeline) {
				// WatchPlugins has already built newPP from the live config
				// (configFn) using s.newRuntime — swap it in and drain the old
				// one. Rebuilding here would compile the whole pipeline twice.
				old := s.pluginPipeline.Swap(newPP)
				if old != nil {
					go old.(*plugin.PluginPipeline).DrainAndClose()
				}
			}, func() { close(watchDone) }); err != nil {
				log.Printf("plugin watcher: %v", err)
				close(watchDone)
			}
		}
	}

	// --- reverse proxy ---------------------------------------------------
	// Context keys for stashing format and chat between Director and ModifyResponse.
	type formatCtxKey struct{}

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

			// The response pipeline reads upstream bodies as plaintext. If the
			// caller's Accept-Encoding (e.g. Claude Code's gzip) were forwarded,
			// Go's transport would pass the compressed body through untouched —
			// json.Unmarshal fails and the response silently bypasses every
			// hook (caught live: plugin-injected fields leaked back to the
			// harness on every non-streamed response). Force identity for any
			// request we intend to parse.
			req.Header.Set("Accept-Encoding", "identity")

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
						reqStateFrom(req.Context()).Verdict = "block"
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
						rs.Verdict = "respond"
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

			// Content-based routing: a plugin holding env.route_request may
			// redirect this request to another configured provider (same wire
			// format) and/or override the model. Applied AFTER identity
			// extraction so rate limiting still keys on the caller, and
			// before marshal so the model override reaches the wire. Bad
			// verdicts fail open to the original route.
			if pl := reqStateFrom(req.Context()).Pipeline; pl != nil && pl.HasGrant("env.route_request") && chat.ToranaMeta != nil {
				if raw, ok := chat.ToranaMeta["_route"]; ok {
					delete(chat.ToranaMeta, "_route")
					applyRoute(req, chat, prov.Format, provName, raw, currentCfg.Providers)
					// Model may have been overridden; refresh the metrics fact.
					rstate := reqStateFrom(req.Context())
					rstate.Model = chat.Model
					rstate.Verdict = "route"
				}
			}

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
			ctx = context.WithValue(ctx, engine.ChatRequestKey, chat)
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
								// Merge rather than overwrite: Anthropic reports
								// input (+cache) tokens on message_start and
								// output tokens on message_delta as separate
								// usage frames.
								rs.mergeUsage(ev.Usage)
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

				// Pin the pipeline for the background goroutine's entire
				// lifetime. The goroutine outlives this handler (it keeps
				// running while ReverseProxy copies pr→client and then calls
				// RunAfterResponse). RunAfterResponse does its own Acquire, but
				// there's a window after the handler's deferred Release where
				// the wg counter can hit 0 — letting a concurrent
				// DrainAndClose().Wait() unblock and race Add(1) (data race +
				// use of a closing runtime). An explicit Acquire/Release around
				// the goroutine keeps the counter above 0 until it's fully done.
				var streamPl *plugin.PluginPipeline
				if pl := reqStateFrom(resp.Request.Context()).Pipeline; pl != nil {
					pl.Acquire()
					streamPl = pl
				}
				pr, pw := io.Pipe()
				go func() {
					if streamPl != nil {
						defer streamPl.Release()
					}
					serErr := fmt.Stream.SerializeStream(resp.Request.Context(), pw, events)
					pw.Close()
					// On client disconnect the request context is cancelled, so
					// the transport tears down the upstream connection and the
					// provider stops generating (see TestClientDisconnectCancels
					// Upstream). ReverseProxy also closes pr, ending
					// SerializeStream early. Belt-and-suspenders: close the
					// upstream body, then drain any events ParseStream still has
					// queued so its goroutine can't be left blocked on an
					// unconsumed send if it wins the race to produce one after
					// SerializeStream returns. On normal completion both are
					// no-ops (body at EOF, channel already closed).
					upstreamBody.Close()
					for range events { //nolint:revive // intentional drain
					}
					if serErr != nil {
						log.Printf("format %s serialize error: %v", fmt.Name, serErr)
					}
					// Observational run_after_response for streaming
					// responses (metrics/audit plugins). Mutations are not
					// applied — the stream has already been written. The
					// _response signal (latency/status/usage) is complete
					// here: the whole stream has been serialized.
					ctx := resp.Request.Context()
					if pl := reqStateFrom(ctx).Pipeline; pl != nil {
						if chat, _ := ctx.Value(engine.ChatRequestKey).(*engine.ChatRequest); chat != nil {
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

				// Defense in depth: some upstreams compress even against
				// Accept-Encoding: identity. A compressed body would fail the
				// JSON parse below and silently bypass every response hook.
				if resp.Header.Get("Content-Encoding") == "gzip" {
					if zr, zerr := gzip.NewReader(bytes.NewReader(bodyBytes)); zerr == nil {
						plain, rerr := io.ReadAll(io.LimitReader(zr, maxBodySize+1))
						zr.Close()
						if rerr == nil && len(plain) <= maxBodySize {
							bodyBytes = plain
							resp.Header.Del("Content-Encoding")
						}
					}
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
					chat, _ := ctx.Value(engine.ChatRequestKey).(*engine.ChatRequest)
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
							rs.mergeUsage(u)
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

	// --- /_torana control-plane namespace --------------------------------
	// These routes MUST be registered before the "/" catch-all so that
	// Go's ServeMux routes them directly and they never reach the provider
	// proxy handler.

	// GET /_torana/api/config — JSON of current effective provider.Config.
	mux.HandleFunc("/_torana/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		b, err := json.Marshal(s.GetConfig().Providers)
		if err != nil {
			http.Error(w, "error marshalling config", http.StatusInternalServerError)
			return
		}
		w.Write(b)
	})

	// PUT /_torana/api/plugins (or POST) — live plugin enable/disable/reorder/edit + persist.
	mux.HandleFunc("/_torana/api/plugins", func(w http.ResponseWriter, r *http.Request) {
		// TODO(controlplane-security): localhost-only guard added in the security workstream
		if r.Method != http.MethodPut && r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Order  *[]string                  `json:"order,omitempty"`
			Config map[string]json.RawMessage `json:"config,omitempty"`
		}
		if r.Body != nil {
			lr := io.LimitReader(r.Body, maxBodySize+1)
			data, err := io.ReadAll(lr)
			if err != nil {
				http.Error(w, "failed to read body", http.StatusBadRequest)
				return
			}
			if len(data) > 0 {
				if err := json.Unmarshal(data, &req); err != nil {
					http.Error(w, "invalid json body", http.StatusBadRequest)
					return
				}
			}
		}

		oldPlugins := s.GetConfig().Providers.Plugins
		newPlugins := oldPlugins
		if req.Order != nil {
			newPlugins.Order = *req.Order
		}
		if req.Config != nil {
			newPlugins.Config = req.Config
		}

		s.configMu.Lock()
		s.config.Providers.Plugins = newPlugins
		s.configMu.Unlock()

		if err := s.RebuildPipeline(newPlugins); err != nil {
			s.configMu.Lock()
			s.config.Providers.Plugins = oldPlugins
			s.configMu.Unlock()
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := s.PersistConfig(); err != nil {
			log.Printf("failed to persist config: %v", err)
			http.Error(w, "failed to persist config to disk", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(newPlugins)
		w.Write(b)
	})

	// GET /_torana/api/feed — one-shot JSON snapshot of recent events,
	// newest-first (up to the ring-buffer capacity, default 200).
	mux.HandleFunc("/_torana/api/feed", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		snap := s.feed.Snapshot()
		if snap == nil {
			// Return an empty JSON array instead of null for API ergonomics.
			w.Write([]byte("[]"))
			return
		}
		b, _ := json.Marshal(snap)
		w.Write(b)
	})

	// GET /_torana/api/stream — SSE stream of live RequestEvents.
	// On connect the current snapshot is replayed (oldest-to-newest) so the
	// client gets a consistent view, then new events are pushed as they arrive.
	// The stream honors request-context cancellation (client disconnect).
	mux.HandleFunc("/_torana/api/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		// Subscribe before replaying the snapshot to avoid a race where an
		// event arrives between the Snapshot() call and Subscribe().
		ch, unsub := s.feed.Subscribe()
		defer unsub()

		// Replay existing events oldest-first so the client sees a coherent
		// history in chronological order before live events begin.
		if snap := s.feed.Snapshot(); len(snap) > 0 {
			for i := len(snap) - 1; i >= 0; i-- {
				b, err := json.Marshal(snap[i])
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "data: %s\n\n", b)
			}
			flusher.Flush()
		}

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				// Client disconnected.
				return
			case ev, ok := <-ch:
				if !ok {
					// Channel closed by unsub (shouldn't happen before ctx cancel,
					// but handle it gracefully).
					return
				}
				b, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
			}
		}
	})

	// /_torana/plugin/<name>/* — per-plugin HTTP namespace.
	//
	// Plugins that declare the run_on_http_request hook and the env.serve_http
	// permission can serve their own HTTP UI/API under this prefix. The route
	// is NON-chat: it does NOT go through the Director or ReverseProxy.
	mux.HandleFunc("/_torana/plugin/", func(w http.ResponseWriter, r *http.Request) {
		// Parse plugin name: first path segment after /_torana/plugin/.
		rest := strings.TrimPrefix(r.URL.Path, "/_torana/plugin/")
		var pluginName, pluginRelPath string
		if idx := strings.IndexByte(rest, '/'); idx >= 0 {
			pluginName = rest[:idx]
			pluginRelPath = rest[idx:] // retains the leading '/'
		} else {
			pluginName = rest
			pluginRelPath = "/"
		}
		if pluginName == "" {
			http.Error(w, "plugin name required", http.StatusNotFound)
			return
		}

		// Load the pinned pipeline. No pipeline → service unavailable.
		raw := s.pluginPipeline.Load()
		if raw == nil {
			http.Error(w, "plugin pipeline not available", http.StatusServiceUnavailable)
			return
		}
		pp := raw.(*plugin.PluginPipeline)
		if !pp.TryAcquire() {
			http.Error(w, "plugin pipeline draining", http.StatusServiceUnavailable)
			return
		}
		defer pp.Release()

		// Build the pb.HttpRequest from the incoming net/http request.
		var bodyBytes []byte
		if r.Body != nil {
			lr := io.LimitReader(r.Body, maxBodySize+1)
			var readErr error
			bodyBytes, readErr = io.ReadAll(lr)
			r.Body.Close()
			if readErr != nil {
				http.Error(w, "read body", http.StatusInternalServerError)
				return
			}
			if int64(len(bodyBytes)) > maxBodySize {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
		}
		headersJSON, _ := json.Marshal(map[string][]string(r.Header))
		httpReq := &pb.HttpRequest{
			Method:      r.Method,
			Path:        pluginRelPath,
			HeadersJson: headersJSON,
			Body:        bodyBytes,
		}

		reqID := reqCounter.Add(1)
		resp, err := pp.RunOnHTTPRequest(r.Context(), reqID, pluginName, httpReq)
		if err != nil {
			if errors.Is(err, plugin.ErrServeHTTPForbidden) {
				http.Error(w, "plugin lacks env.serve_http permission", http.StatusForbidden)
				return
			}
			log.Printf("[proxy] /_torana/plugin/%s: %v", pluginName, err)
			http.Error(w, "plugin dispatch error", http.StatusServiceUnavailable)
			return
		}
		if resp == nil {
			http.Error(w, "plugin not found or did not handle request", http.StatusNotFound)
			return
		}

		// Write the plugin's response.
		if len(resp.HeadersJson) > 0 {
			var hdrs map[string][]string
			if err := json.Unmarshal(resp.HeadersJson, &hdrs); err == nil {
				for k, vals := range hdrs {
					for _, v := range vals {
						w.Header().Add(k, v)
					}
				}
			}
		}
		status := int(resp.Status)
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if len(resp.Body) > 0 {
			w.Write(resp.Body)
		}
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
			candidate := pp.(*plugin.PluginPipeline)
			if candidate.TryAcquire() {
				rs.Pipeline = candidate
			}
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
		s.stats.RecordCacheTokens(int64(rs.UsageCacheRead), int64(rs.UsageCacheWrite))
		// Host request metrics: latency + outcome, labeled by model/provider.
		// The host sees every response (including errors and vetoes), so this
		// is the reliable source of truth for latency and status.
		latencyMS := float64(time.Since(rs.Start).Microseconds()) / 1000
		metrics.RecordProxyRequest(r.Context(), rs.Model, rs.Provider, tw.status, latencyMS)
		metrics.RecordTokens(r.Context(), rs.Model, rs.Provider, rs.UsageIn, rs.UsageOut)
		metrics.RecordCacheTokens(r.Context(), rs.Model, rs.Provider, rs.UsageCacheRead, rs.UsageCacheWrite)
		// Record a per-request event in the live feed (control-plane dashboard).
		// Add is O(1) and non-blocking — it never stalls the request goroutine.
		// TODO(controlplane): populate Plugins once the pipeline exposes which
		// plugins ran for this request ID.
		s.feed.Add(metrics.RequestEvent{
			Timestamp:        rs.Start.UTC().Format(time.RFC3339Nano),
			Provider:         rs.Provider,
			Model:            rs.Model,
			Status:           tw.status,
			LatencyMS:        latencyMS,
			TokensIn:         int64(rs.UsageIn),
			TokensOut:        int64(rs.UsageOut),
			CacheReadTokens:  int64(rs.UsageCacheRead),
			CacheWriteTokens: int64(rs.UsageCacheWrite),
			BytesIn:          tr.bytesRead,
			BytesOut:         tw.bytesWritten,
			Verdict:          rs.Verdict,
		})
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

// Handler returns the server's HTTP handler (the provider-routing mux). The
// MITM ingress delegates decrypted chat requests to it so they run through the
// full plugin pipeline exactly like a direct /provider/<name>/… call.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// SetProviders hot-reloads the provider configuration without restarting.
func (s *Server) SetProviders(cfg provider.Config) {
	s.configMu.Lock()
	s.config.Providers = cfg
	s.configMu.Unlock()
	s.rateLimiter.Update(cfg.Limits.RPM, cfg.Limits.Concurrency)
	log.Printf("config hot-reload: %d providers loaded", len(cfg.Providers))
}

// newRuntime wires host callbacks for a WASM runtime.
func (s *Server) newRuntime() *wasm.Runtime {
	rt := wasm.NewRuntimeWithCache(context.Background(), s.sharedCache)
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

// RebuildPipeline builds a fresh runtime + plugin pipeline using pcfg,
// then atomically swaps the active pipeline and drains the old one.
// If reloading fails (e.g. ordering constraint violation), returns the error
// without swapping the active pipeline.
func (s *Server) RebuildPipeline(pcfg provider.PluginsConfig) error {
	s.rebuildMu.Lock()
	defer s.rebuildMu.Unlock()

	rt := s.newRuntime()
	pp, err := plugin.NewPipeline(rt, plugin.PluginConfig{
		Dir:    pcfg.Dir,
		Order:  pcfg.Order,
		Config: pcfg.Config,
	})
	if err != nil {
		rt.Close()
		return err
	}

	old := s.pluginPipeline.Swap(pp)
	if old != nil {
		go old.(*plugin.PluginPipeline).DrainAndClose()
	}
	return nil
}

// PersistConfig saves the current in-memory provider configuration to disk.
// The 5s modtime poller (provider.WatchConfig) will observe Save()'s write
// and call SetProviders again (benign; it does not rebuild the pipeline).
// Atomic rename prevents a half-written read.
func (s *Server) PersistConfig() error {
	path := s.configPath
	if path == "" {
		path = "config.json"
	}
	return provider.Save(path, s.GetConfig().Providers)
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
	if s.watchDone != nil {
		<-s.watchDone
	}
	// Stop accepting new requests and let HTTP cancellation unblock streams
	// before waiting for their pinned plugin pipeline.
	if s.httpServer != nil {
		if err := s.httpServer.Shutdown(ctx); err != nil {
			return err
		}
	}
	if pp := s.pluginPipeline.Load(); pp != nil {
		pp.(*plugin.PluginPipeline).DrainAndClose()
	}
	s.rateLimiter.Close()
	if s.sharedCache != nil {
		s.sharedCache.Close()
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
