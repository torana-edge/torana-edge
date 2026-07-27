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
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http/httpguts"
	"google.golang.org/protobuf/proto"

	"github.com/torana-edge/torana-edge/internal/cache"
	"github.com/torana-edge/torana-edge/internal/controlplane"
	"github.com/torana-edge/torana-edge/internal/conversation"
	"github.com/torana-edge/torana-edge/internal/economics"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/mitm"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/pluginstate"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/secret"
	"github.com/torana-edge/torana-edge/internal/wasm"
	"github.com/torana-edge/torana-plugin-sdk/pb"
)

const maxBodySize = 10 * 1024 * 1024 // 10 MB
const secretSetSentinel = "__set__"
const maxPluginResponseHeaders = 64
const maxPluginResponseHeaderBytes = 16 * 1024

var allowedPluginResponseHeaders = map[string]struct{}{
	"Content-Language": {},
	"Content-Type":     {},
}

func applyPluginResponseHeaders(dst http.Header, encoded []byte) error {
	if len(encoded) == 0 {
		return nil
	}
	if len(encoded) > maxPluginResponseHeaderBytes {
		return fmt.Errorf("encoded response headers too large")
	}
	var headers map[string][]string
	if err := json.Unmarshal(encoded, &headers); err != nil {
		return err
	}
	if len(headers) > maxPluginResponseHeaders {
		return fmt.Errorf("too many response headers")
	}
	total := 0
	for name, values := range headers {
		canonical := http.CanonicalHeaderKey(name)
		if canonical == "" || !httpguts.ValidHeaderFieldName(canonical) {
			return fmt.Errorf("invalid header name")
		}
		if _, allowed := allowedPluginResponseHeaders[canonical]; !allowed {
			return fmt.Errorf("response header %q is not allowed", canonical)
		}
		for _, value := range values {
			total += len(canonical) + len(value)
			if total > maxPluginResponseHeaderBytes {
				return fmt.Errorf("response headers too large")
			}
			if !httpguts.ValidHeaderFieldValue(value) {
				return fmt.Errorf("invalid value for header %q", canonical)
			}
			dst.Add(canonical, value)
		}
	}
	return nil
}

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
	listenerMu sync.Mutex
	mitmMu     sync.Mutex
	listener   net.Listener
	mitmSrv    *mitm.Server
	bindHost   string
	configPath string
	config     Config
	secrets    *secret.Store
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
	// cacheMu guards sharedCache: ReconfigureCache may swap it at runtime while
	// the plugin-watcher goroutine reads it via newRuntime (off rebuildMu).
	cacheMu     sync.RWMutex
	rateLimiter *RateLimiter
	// conversations tracks recently seen conversations (metadata only, never
	// prompt content) so the control plane and plugins can name one. Bounded
	// and self-expiring; closed on Shutdown.
	conversations *conversation.Registry
	// watchCancel stops the plugin hot-reload watcher on Shutdown.
	watchCancel context.CancelFunc
	watchDone   <-chan struct{}
	// ticker drives run_on_tick. Nil when ticks are not configured.
	ticker *tickScheduler
	// pluginState is durable per-plugin storage (env.state_*), kept beside the
	// managed config. Nil when there is no config path to anchor it to.
	pluginState *pluginstate.Store
	// egress meters plugin-originated provider requests against per-plugin
	// budgets, so a plugin cannot spend without a ceiling an operator set.
	egress *egressMeter
}

type routeContextKey struct{}

func isOpenAIResponsesRequest(chat *engine.ChatRequest) bool {
	if chat == nil || chat.ProviderExtensions == nil {
		return false
	}
	variant, _ := chat.ProviderExtensions["_openai_variant"].(string)
	return variant == "responses"
}

func applyOpenAIResponsesCompaction(chat *engine.ChatRequest, p provider.Provider) {
	if !isOpenAIResponsesRequest(chat) || p.ResponsesCompaction == nil {
		return
	}
	if _, supplied := chat.ProviderExtensions["context_management"]; supplied {
		return
	}
	chat.ProviderExtensions["context_management"] = []any{map[string]any{
		"type":              "compaction",
		"compact_threshold": p.ResponsesCompaction.CompactThreshold,
	}}
}

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
	// ConversationID is the durable label for the conversation this request
	// belongs to, derived from the canonical IR before any plugin runs.
	// Empty when the request could not be identified.
	ConversationID string
	// CachePrefixKey fingerprints the provider-side cache entry this request
	// will hit, computed after routing and plugin mutation so it describes what
	// actually goes on the wire. Unlike ConversationID it moves whenever the
	// prefix or model changes.
	CachePrefixKey string
	// Path is the provider-stripped request path, kept because the proxy never
	// synthesizes a chat path and anything replaying this conversation needs
	// the one the caller used.
	Path string
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
	// CompactionReports are queued by request-side WASM host calls and priced
	// only after routing has selected the final provider/model.
	CompactionReports          []attributedCompactionReport
	InitialProvider            string
	InitialFormat              string
	PendingRoute               *routeVerdict
	CompactionRequestPrepared  bool
	CompactionReportsCommitted bool
}

type attributedCompactionReport struct {
	Plugin string
	Report economics.CompactionReport
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

// recordCompactionReports resolves prices after routing. Missing providers,
// models, or rates are intentionally passed through as nil so /stats records
// an explicit unavailable reason rather than a guessed dollar value.
func (s *Server) recordCompactionReports(rs *reqState) {
	if rs == nil || !rs.CompactionReportsCommitted || len(rs.CompactionReports) == 0 {
		return
	}
	defer func() {
		rs.CompactionReports = nil
		rs.CompactionReportsCommitted = false
	}()
	for _, attributed := range rs.CompactionReports {
		report := attributed.Report
		targetPricing, offloadPricing := s.compactionPricing(rs, report)
		s.stats.RecordCompactionReport(attributed.Plugin, report, targetPricing, offloadPricing)
		metrics.RecordPluginSavings(context.Background(), attributed.Plugin, report.OriginalBytes-report.FinalBytes)
		metrics.RecordCompactionEconomics(context.Background(), attributed.Plugin, report, targetPricing, offloadPricing)
	}
}

func discardCompactionReports(rs *reqState) {
	if rs == nil {
		return
	}
	rs.CompactionReports = nil
	rs.CompactionRequestPrepared = false
	rs.CompactionReportsCommitted = false
}

func (s *Server) compactionPricing(rs *reqState, report economics.CompactionReport) (*economics.ModelPricing, *economics.ModelPricing) {
	cfg := s.GetConfig().Providers
	providerName, model := report.Provider, report.Model
	if providerName == "" && rs != nil {
		providerName = rs.Provider
	}
	if model == "" && rs != nil {
		model = rs.Model
	}
	var targetPricing *economics.ModelPricing
	if prov, ok := cfg.Providers[providerName]; ok {
		if price, ok := prov.PricingFor(model); ok {
			targetPricing = &price
		}
	}
	var offloadPricing *economics.ModelPricing
	if report.Offload != nil {
		if prov, ok := cfg.Providers[report.Offload.Provider]; ok {
			if price, ok := prov.PricingFor(report.Offload.Model); ok {
				offloadPricing = &price
			}
		}
	}
	return targetPricing, offloadPricing
}

func (s *Server) evaluateCompaction(ctx context.Context, report economics.CompactionReport) economics.CompactionDecision {
	rs := reqStateFrom(ctx)
	targetName := report.Provider
	if targetName == "" {
		targetName = rs.Provider
	}
	if rs.PendingRoute != nil {
		cfg := s.GetConfig().Providers
		targetName = rs.PendingRoute.Provider
		if targetName == "" {
			targetName = rs.InitialProvider
		}
		targetProvider, ok := cfg.Providers[targetName]
		if !ok || (rs.InitialFormat != "" && targetProvider.Format != rs.InitialFormat) {
			return economics.CompactionDecision{Reason: economics.UnavailableRouteUnresolved}
		}
		report.Provider = targetName
		if rs.PendingRoute.Model != "" {
			report.Model = rs.PendingRoute.Model
		}
	}
	target, offload := s.compactionPricing(rs, report)
	decision := economics.DecideCompaction(report, target, offload)
	if !decision.Apply {
		return decision
	}

	// A request may ultimately run on any configured fallback. Fail closed if
	// one of those routes is wire-incompatible, unpriced, or would make the
	// same batch uneconomic; otherwise a primary-priced decision could produce
	// an optimistic claim after failover.
	cfg := s.GetConfig().Providers
	primary, ok := cfg.Providers[targetName]
	if !ok {
		return economics.CompactionDecision{Reason: economics.UnavailableRouteUnresolved}
	}
	for _, fallbackName := range fallbackNamesForProvider(targetName, cfg) {
		fallback, ok := cfg.Providers[fallbackName]
		if !ok || (primary.Format != "" && fallback.Format != "" && fallback.Format != primary.Format) {
			return economics.CompactionDecision{Reason: economics.UnavailableFallbackUnpriced}
		}
		price, ok := fallback.PricingFor(report.Model)
		if !ok {
			model := report.Model
			if model == "" {
				model = rs.Model
			}
			price, ok = fallback.PricingFor(model)
		}
		if !ok || !economics.DecideCompaction(report, &price, offload).Apply {
			return economics.CompactionDecision{Reason: economics.UnavailableFallbackUnpriced}
		}
	}
	return decision
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
	secStore, err := secret.Open(filepath.Dir(configPath))
	if err != nil {
		return nil, fmt.Errorf("proxy: secret store: %w", err)
	}
	// Durable plugin state lives beside the managed config. A failure to load
	// it is reported but not fatal: the store still works in memory, and
	// refusing to start the proxy because one plugin's scratch file was
	// truncated would be a poor trade.
	stateStore, err := pluginstate.New(pluginstate.Options{
		Path: filepath.Join(filepath.Dir(configPath), "plugin-state.json"),
	})
	if err != nil {
		log.Printf("warning: %v", err)
	}
	s := &Server{
		config:        cfg,
		configPath:    configPath,
		secrets:       secStore,
		stats:         statsTracker,
		feed:          metrics.NewRequestFeed(0), // default 200-event ring buffer
		rateLimiter:   NewRateLimiter(cfg.Providers.Limits.RPM, cfg.Providers.Limits.Concurrency),
		conversations: conversation.New(conversation.Options{}),
		pluginState:   stateStore,
		egress:        newEgressMeter(),
	}

	// --- offload validation (fail fast on misconfiguration) ---------------
	if err := cfg.Providers.Offload.Validate(cfg.Providers.Providers); err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	for name, p := range cfg.Providers.Providers {
		if err := p.ValidateResponsesCompaction(name); err != nil {
			return nil, fmt.Errorf("proxy: %w", err)
		}
		if err := p.ValidateCache(name); err != nil {
			return nil, fmt.Errorf("proxy: %w", err)
		}
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
		cfg.Providers.Cache.Redis.Password = s.resolveSecret(cfg.Providers.Cache.Redis.PasswordEnv, cfg.Providers.Cache.Redis.PasswordEnc)
		sharedCache, err := cache.New(cfg.Providers.Cache)
		if err != nil {
			return nil, fmt.Errorf("proxy: %w", err)
		}
		s.setCache(sharedCache)

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
				return plugin.PluginConfig{
					Dir:             p.Dir,
					Order:           p.Order,
					Config:          p.Config,
					Approvals:       pluginApprovals(p.Approvals),
					AllowUnapproved: p.AllowUnapproved,
					Strict:          true,
				}
			}
			watchDone := make(chan struct{})
			s.watchDone = watchDone
			runtimeFn := func() *wasm.Runtime {
				s.rebuildMu.Lock()
				defer s.rebuildMu.Unlock()
				return s.newRuntime()
			}
			if err := plugin.WatchPlugins(watchCtx, cfg.Providers.Plugins.Dir, configFn, runtimeFn, func(newPP *plugin.PluginPipeline) {
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
		// Rewrite, not Director: Director is deprecated as of Go 1.26. The body
		// below is unchanged — req is pr.Out, the clone ReverseProxy builds for
		// the upstream, which is exactly what Director received. Two behavioural
		// differences come from net/http/httputil itself, both wanted here:
		// inbound X-Forwarded-* and Forwarded headers are stripped rather than
		// appended to (this proxy talks to LLM providers; a caller's IP chain has
		// no business being forwarded to them, and we deliberately do not call
		// pr.SetXForwarded), and unparsable query parameters are dropped.
		Rewrite: func(pr *httputil.ProxyRequest) {
			req := pr.Out
			var body []byte
			if req.Body != nil {
				// ServeHTTP has already enforced maxBodySize with a
				// MaxBytesReader and returned 413, so nothing oversized reaches
				// here. There used to be a second check that replaced an
				// oversized body with an EMPTY one and forwarded it — dead code
				// that an external auditor reasonably read as a live bug
				// ("body silently truncated, upstream 400s"). The limit belongs
				// at the edge, where it can still return a status code; here it
				// could only corrupt the request.
				lr := io.LimitReader(req.Body, maxBodySize+1)
				body, _ = io.ReadAll(lr)
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
			// Economic-gate host calls run inside request hooks, so make the
			// initially routed provider/model available before the pipeline.
			// A later content-routing verdict refreshes these fields as before.
			rs := reqStateFrom(req.Context())
			rs.Provider = provName
			rs.Model = chat.Model
			rs.InitialProvider = provName
			rs.InitialFormat = prov.Format

			// Label the conversation from the canonical IR, before any plugin
			// can rewrite the messages it is derived from. Deriving it after
			// RunBeforeRequest would let a compactor rename the conversation it
			// just compacted, which is precisely the case the label exists to
			// survive. The cache-prefix key is deliberately computed later.
			rs.ConversationID = engine.ConversationID(chat)

			// Publish the routing decision so plugins can ask the host about
			// this provider — pricing and cache semantics are keyed by provider
			// name, and a plugin that cannot name its own provider cannot look
			// up the economics of what it is about to do. ToranaMeta never
			// reaches the wire and is excluded from the determinism check, so
			// this is safe to vary per request.
			chat.ToranaMeta["_provider"] = provName
			chat.ToranaMeta["_conversation_id"] = rs.ConversationID
			// The path too: Torana forwards whatever the caller sent rather
			// than synthesizing one, so a plugin replaying this conversation
			// has no other way to know where it goes.
			chat.ToranaMeta["_path"] = strippedPath

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
						discardCompactionReports(reqStateFrom(req.Context()))
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
						discardCompactionReports(rs)
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
					s.applyRoute(req, chat, prov.Format, provName, raw, currentCfg.Providers)
					// Model may have been overridden; refresh the metrics fact.
					rstate := reqStateFrom(req.Context())
					rstate.Model = chat.Model
					rstate.Verdict = "route"
				}
			}

			// OpenAI can compact server-managed Responses history without Torana
			// rewriting an opaque previous_response_id chain. This is opt-in per
			// provider, applies only to Responses requests, and never overrides a
			// caller-supplied context_management policy.
			if fmt.Name == "openai" && isOpenAIResponsesRequest(chat) {
				routedProvider := currentCfg.Providers.Providers[rc.ProviderName]
				applyOpenAIResponsesCompaction(chat, routedProvider)
			}

			// Token usage on openai streams is opt-in; opt in on the caller's
			// behalf so the host can meter tokens. The resulting usage frame
			// is consumed host-side and suppressed from the client's stream
			// (see the usage tap in ModifyResponse) — unless the client asked
			// for it itself, in which case nothing is injected or suppressed.
			if fmt.Name == "openai" && chat.Stream && !isOpenAIResponsesRequest(chat) {
				if _, ok := chat.ProviderExtensions["stream_options"]; !ok {
					if chat.ProviderExtensions == nil {
						chat.ProviderExtensions = map[string]any{}
					}
					chat.ProviderExtensions["stream_options"] = map[string]any{"include_usage": true}
					reqStateFrom(req.Context()).UsageInjected = true
				}
			}

			// Fingerprint the cache prefix as it will actually go on the wire:
			// after content routing may have changed the model, and after every
			// plugin has had its say. Computing it earlier would key the entry
			// on a request that was never sent.
			rs.CachePrefixKey = engine.CachePrefixKey(chat)
			rs.Path = rc.StrippedPath

			newBody, err := fmt.Request.Marshal(chat)
			if err != nil {
				log.Printf("format %s marshal error: %v — passing through", fmt.Name, err)
				newBody = body
				// The original request is sent, so queued reports about plugin
				// mutations must never become savings metrics.
				discardCompactionReports(reqStateFrom(req.Context()))
			} else {
				reqStateFrom(req.Context()).CompactionRequestPrepared = true
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
	statsHandler := s.controlPlaneGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(s.stats)
		w.Write(b)
	})
	// Usage data can reveal provider, model, and traffic information. Keep the
	// legacy endpoint for local scripts, but protect it like the dashboard.
	mux.HandleFunc("/stats", statsHandler)

	// --- /_torana control-plane namespace --------------------------------
	// These routes MUST be registered before the "/" catch-all so that
	// Go's ServeMux routes them directly and they never reach the provider
	// proxy handler.

	// GET /_torana/api/config — JSON of current effective provider.Config.
	mux.HandleFunc("/_torana/api/config", s.controlPlaneGuard(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			cfg := redactConfigSecrets(s.GetConfig().Providers)
			b, err := json.Marshal(cfg)
			if err != nil {
				http.Error(w, "error marshalling config", http.StatusInternalServerError)
				return
			}
			w.Write(b)

		case http.MethodPut, http.MethodPost:
			// Settings write-back: providers / offload / limits / control_plane.
			// The plugin pipeline (order + per-plugin config) is owned by
			// /_torana/api/plugins and is preserved verbatim here.
			lr := io.LimitReader(r.Body, maxBodySize+1)
			data, err := io.ReadAll(lr)
			if err != nil {
				http.Error(w, "failed to read body", http.StatusBadRequest)
				return
			}
			var incoming provider.Config
			if err := json.Unmarshal(data, &incoming); err != nil {
				http.Error(w, "invalid json body", http.StatusBadRequest)
				return
			}
			cur := s.GetConfig().Providers
			// Never let the settings surface mutate the pipeline.
			incoming.Plugins = cur.Plugins
			// Preserve the redacted control-plane token when the client
			// echoes back an empty one.
			if incoming.ControlPlane.Token == "" {
				incoming.ControlPlane.Token = cur.ControlPlane.Token
			}
			// Normalize encrypted secrets (*_enc fields)
			offEnc, err := s.normalizeSecretField(incoming.Offload.APIKeyEnc, cur.Offload.APIKeyEnc)
			if err != nil {
				http.Error(w, "failed to encrypt secret", http.StatusInternalServerError)
				return
			}
			incoming.Offload.APIKeyEnc = offEnc

			cacheEnc, err := s.normalizeSecretField(incoming.Cache.Redis.PasswordEnc, cur.Cache.Redis.PasswordEnc)
			if err != nil {
				http.Error(w, "failed to encrypt secret", http.StatusInternalServerError)
				return
			}
			incoming.Cache.Redis.PasswordEnc = cacheEnc

			// Carry forward per-provider fields the caller did not mention.
			// The settings form renders only six fields per provider and sends
			// a rebuilt object, so without this an ordinary Save silently drops
			// pricing and cache semantics — which the compactor's economic gate
			// and the cache plugins depend on. Absent means preserve; an
			// explicit null or empty value still clears, so the fields stay
			// editable by any client that actually manages them.
			preserveUnmanagedProviderFields(cur.Providers, incoming.Providers, providerFieldsSent(data))

			if incoming.Providers != nil {
				for name, incP := range incoming.Providers {
					curP := cur.Providers[name]
					pEnc, err := s.normalizeSecretField(incP.APIKeyEnc, curP.APIKeyEnc)
					if err != nil {
						http.Error(w, fmt.Sprintf("failed to encrypt secret for provider %s", name), http.StatusInternalServerError)
						return
					}
					incP.APIKeyEnc = pEnc
					incoming.Providers[name] = incP
				}
			}

			incoming.Managed = true
			if err := incoming.Validate(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			if err := s.applyProviderConfigTransaction(cur, incoming); err != nil {
				log.Printf("failed to apply config transaction: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			out := redactConfigSecrets(incoming)
			b, _ := json.Marshal(out)
			w.Write(b)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	// PUT /_torana/api/plugins (or POST) — live plugin enable/disable/reorder/edit + persist.
	mux.HandleFunc("/_torana/api/plugins", s.controlPlaneGuard(func(w http.ResponseWriter, r *http.Request) {
		// GET — enumerate every plugin discovered on disk, marking which are
		// enabled (present in plugins.order) and which serve their own HTTP UI.
		if r.Method == http.MethodGet {
			cur := s.GetConfig().Providers.Plugins
			orderIdx := make(map[string]int, len(cur.Order))
			for i, n := range cur.Order {
				orderIdx[n] = i
			}
			type pluginInfo struct {
				ID           string                   `json:"id"`
				Name         string                   `json:"name"`
				Version      string                   `json:"version"`
				Digest       string                   `json:"digest"`
				FailureMode  string                   `json:"failure_mode"`
				Description  string                   `json:"description"`
				Hooks        []string                 `json:"hooks"`
				Permissions  []string                 `json:"permissions"`
				Enabled      bool                     `json:"enabled"`
				Order        int                      `json:"order"`
				State        string                   `json:"state"`
				Loaded       bool                     `json:"loaded"`
				LoadedDigest string                   `json:"loaded_digest,omitempty"`
				ServesHTTP   bool                     `json:"serves_http"`
				Schema       *plugin.ConfigSchema     `json:"schema,omitempty"`
				Agent        *plugin.AgentDescriptor  `json:"agent,omitempty"`
				LoadedAgent  *plugin.AgentDescriptor  `json:"loaded_agent,omitempty"`
				Config       json.RawMessage          `json:"config,omitempty"`
				Approval     *provider.PluginApproval `json:"approval,omitempty"`
			}
			loadedByName := make(map[string]plugin.LoadedPluginStatus)
			if rawPipeline := s.pluginPipeline.Load(); rawPipeline != nil {
				pipeline := rawPipeline.(*plugin.PluginPipeline)
				if pipeline.TryAcquire() {
					for _, status := range pipeline.LoadedPlugins() {
						loadedByName[status.Name] = status
					}
					pipeline.Release()
				}
			}
			bundles, discoveryErr := plugin.DiscoverPlugins(cur.Dir)
			if discoveryErr != nil {
				http.Error(w, "plugin discovery failed", http.StatusInternalServerError)
				return
			}
			infos := make([]pluginInfo, 0, len(bundles))
			seen := make(map[string]bool, len(bundles))
			for _, b := range bundles {
				m := b.Manifest
				hooks := make([]string, 0, len(m.Hooks))
				for _, h := range m.Hooks {
					hooks = append(hooks, h.Name)
				}
				perms := make([]string, 0, len(m.Permissions))
				for _, p := range m.Permissions {
					perms = append(perms, p.Name)
				}
				idx, enabled := orderIdx[m.Name]
				approval, approved := cur.Approvals[m.ID]
				if !approved {
					approval, approved = cur.Approvals[m.Name]
				}
				var approvalPtr *provider.PluginApproval
				if approved {
					copy := approval
					copy.Permissions = append([]string(nil), approval.Permissions...)
					approvalPtr = &copy
				}
				loadedStatus, loaded := loadedByName[m.Name]
				state := "disabled"
				if enabled {
					state = "unavailable"
					if loaded {
						state = "current"
						if loadedStatus.Digest != b.Digest {
							state = "stale"
						}
					}
				}
				infos = append(infos, pluginInfo{
					ID:           m.ID,
					Name:         m.Name,
					Version:      m.Version,
					Digest:       b.Digest,
					FailureMode:  m.FailureMode,
					Description:  m.Description,
					Hooks:        hooks,
					Permissions:  perms,
					Enabled:      enabled,
					Order:        idx,
					State:        state,
					Loaded:       loaded,
					LoadedDigest: loadedStatus.Digest,
					ServesHTTP:   loaded && loadedStatus.ServesHTTP,
					Schema:       b.Schema,
					Agent:        b.Agent,
					LoadedAgent:  loadedStatus.Agent,
					Config:       cur.Config[m.Name],
					Approval:     approvalPtr,
				})
				seen[m.Name] = true
			}
			// Surface enabled-but-not-on-disk plugins so the operator can still
			// see and remove a stale pipeline entry from the UI.
			for _, n := range cur.Order {
				if !seen[n] {
					loadedStatus, loaded := loadedByName[n]
					infos = append(infos, pluginInfo{
						Name:         n,
						Enabled:      true,
						Order:        orderIdx[n],
						State:        "missing",
						Loaded:       loaded,
						LoadedDigest: loadedStatus.Digest,
						ServesHTTP:   loaded && loadedStatus.ServesHTTP,
						Hooks:        []string{},
						Permissions:  []string{},
						LoadedAgent:  loadedStatus.Agent,
						Config:       cur.Config[n],
					})
				}
			}
			w.Header().Set("Content-Type", "application/json")
			b, _ := json.Marshal(struct {
				Dir     string       `json:"dir"`
				Plugins []pluginInfo `json:"plugins"`
			}{Dir: cur.Dir, Plugins: infos})
			w.Write(b)
			return
		}

		if r.Method != http.MethodPut && r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Order     *[]string                           `json:"order,omitempty"`
			Config    map[string]json.RawMessage          `json:"config,omitempty"`
			Approvals *map[string]provider.PluginApproval `json:"approvals,omitempty"`
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
			// Validate against each bundle's declared schema, exactly as the
			// per-plugin POST does. Only that endpoint checked, while the
			// dashboard's "Save & apply" button uses THIS one — so the path
			// operators actually click was the unvalidated one, and a
			// wrong-typed value reached the guest to misbehave silently.
			bundles, _ := plugin.DiscoverPlugins(oldPlugins.Dir)
			schemaByName := make(map[string]*plugin.ConfigSchema, len(bundles))
			for _, b := range bundles {
				schemaByName[b.Manifest.Name] = b.Schema
			}
			for name, raw := range req.Config {
				schema, ok := schemaByName[name]
				if !ok {
					// No bundle on disk to check against. Configuration for
					// absent plugins is deliberately retained, so this is not
					// an error — it matches the per-plugin endpoint.
					continue
				}
				if verr := plugin.ValidateConfigAgainstSchema(schema, raw); verr != nil {
					http.Error(w, fmt.Sprintf("plugin %q: %v", name, verr), http.StatusBadRequest)
					return
				}
			}

			// PATCH-like semantics deliberately retain configurations for disabled
			// plugins. The old dashboard only submits enabled plugins, and replacing
			// this map silently discarded a disabled plugin's settings.
			newPlugins.Config = clonePluginConfig(oldPlugins.Config)
			for name, raw := range req.Config {
				newPlugins.Config[name] = raw
			}
		}
		if req.Approvals != nil {
			newPlugins.Approvals = make(map[string]provider.PluginApproval, len(*req.Approvals))
			for id, approval := range *req.Approvals {
				approval.Permissions = append([]string(nil), approval.Permissions...)
				newPlugins.Approvals[id] = approval
			}
		}

		candidate := s.GetConfig().Providers
		candidate.Plugins = newPlugins
		if err := s.persistProviders(candidate); err != nil {
			log.Printf("failed to persist config: %v", err)
			http.Error(w, "failed to persist config to disk", http.StatusInternalServerError)
			return
		}
		skipped, err := s.rebuildPipelineReportingSkips(newPlugins)
		if err != nil {
			if rollbackErr := s.persistProviders(s.GetConfig().Providers); rollbackErr != nil {
				log.Printf("failed to restore config after rejected plugin update: %v", rollbackErr)
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.SetProviders(candidate)

		w.Header().Set("Content-Type", "application/json")
		writePluginsWithWarnings(w, newPlugins, skipped)
	}))

	// POST /_torana/api/plugins/<name>/config — update single plugin config + rebuild + persist.
	mux.HandleFunc("/_torana/api/plugins/", s.controlPlaneGuard(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/_torana/api/plugins/")
		if !strings.HasSuffix(rest, "/config") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		name := strings.TrimSuffix(rest, "/config")
		if name == "" || strings.Contains(name, "/") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		cur := s.GetConfig().Providers.Plugins
		bundles, _ := plugin.DiscoverPlugins(cur.Dir)
		known := false
		for _, b := range bundles {
			if b.Manifest.Name == name {
				known = true
				break
			}
		}
		if !known {
			for _, o := range cur.Order {
				if o == name {
					known = true
					break
				}
			}
		}
		if !known {
			if _, ok := cur.Config[name]; ok {
				known = true
			}
		}
		if !known {
			http.Error(w, "plugin not found", http.StatusNotFound)
			return
		}

		if r.Body == nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		lr := io.LimitReader(r.Body, maxBodySize+1)
		data, err := io.ReadAll(lr)
		if err != nil || len(data) == 0 {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		if !json.Valid(data) {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}

		// Check the blob against the schema the bundle declares. Until this
		// existed, a wrong-typed value reached the guest and misbehaved
		// silently.
		for _, b := range bundles {
			if b.Manifest.Name != name {
				continue
			}
			if verr := plugin.ValidateConfigAgainstSchema(b.Schema, json.RawMessage(data)); verr != nil {
				http.Error(w, verr.Error(), http.StatusBadRequest)
				return
			}
			break
		}

		raw := json.RawMessage(data)
		oldPlugins := s.GetConfig().Providers.Plugins
		newPlugins := oldPlugins
		if newPlugins.Config == nil {
			newPlugins.Config = make(map[string]json.RawMessage)
		} else {
			cfgCopy := make(map[string]json.RawMessage, len(oldPlugins.Config))
			for k, v := range oldPlugins.Config {
				cfgCopy[k] = v
			}
			newPlugins.Config = cfgCopy
		}
		newPlugins.Config[name] = raw

		candidate := s.GetConfig().Providers
		candidate.Plugins = newPlugins
		if err := s.persistProviders(candidate); err != nil {
			log.Printf("failed to persist config: %v", err)
			http.Error(w, "failed to persist config to disk", http.StatusInternalServerError)
			return
		}
		skipped, err := s.rebuildPipelineReportingSkips(newPlugins)
		if err != nil {
			if rollbackErr := s.persistProviders(s.GetConfig().Providers); rollbackErr != nil {
				log.Printf("failed to restore config after rejected plugin update: %v", rollbackErr)
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.SetProviders(candidate)

		w.Header().Set("Content-Type", "application/json")
		writePluginsWithWarnings(w, newPlugins, skipped)
	}))

	// GET /_torana/api/conversations — conversations seen recently, most
	// recently active first. Metadata only: identifiers, timestamps, and token
	// counts, never message content. The /v1/ prefix reaches this through the
	// same shim as every other route.
	mux.HandleFunc("/_torana/api/conversations", s.controlPlaneGuard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		list := s.conversations.List()
		if list == nil {
			// An empty array rather than null, matching /feed.
			list = []conversation.Record{}
		}
		b, _ := json.Marshal(struct {
			Conversations []conversation.Record `json:"conversations"`
		}{Conversations: list})
		w.Write(b)
	}))

	// GET /_torana/api/feed — one-shot JSON snapshot of recent events,
	// newest-first (up to the ring-buffer capacity, default 200).
	mux.HandleFunc("/_torana/api/feed", s.controlPlaneGuard(func(w http.ResponseWriter, r *http.Request) {
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
	}))

	// GET /_torana/api/stream — SSE stream of live RequestEvents.
	// On connect the current snapshot is replayed (oldest-to-newest) so the
	// client gets a consistent view, then new events are pushed as they arrive.
	// The stream honors request-context cancellation (client disconnect).
	mux.HandleFunc("/_torana/api/stream", s.controlPlaneGuard(func(w http.ResponseWriter, r *http.Request) {
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
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Connection", "keep-alive")

		// Subscribe and capture the snapshot atomically under one lock, so an
		// event arriving between snapshot and subscribe is never delivered twice.
		snap, ch, unsub := s.feed.SubscribeWithSnapshot()
		defer unsub()

		// Replay existing events oldest-first so the client sees a coherent
		// history in chronological order before live events begin.
		if len(snap) > 0 {
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
	}))

	// /_torana/plugin/<name>/* — per-plugin HTTP namespace.
	//
	// Plugins that declare the run_on_http_request hook and the env.serve_http
	// permission can serve their own HTTP UI/API under this prefix. The route
	// is NON-chat: it does NOT go through the Director or ReverseProxy.
	mux.HandleFunc("/_torana/plugin/", s.controlPlanePluginGuard(func(w http.ResponseWriter, r *http.Request) {
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

		resp, err := dispatchPluginHTTPRequest(r.Context(), pp, pluginName, httpReq)
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
		if len(resp.Body) > maxBodySize {
			http.Error(w, "plugin response body too large", http.StatusBadGateway)
			return
		}

		if err := applyPluginResponseHeaders(w.Header(), resp.HeadersJson); err != nil {
			http.Error(w, "plugin returned invalid response headers", http.StatusBadGateway)
			return
		}
		status := int(resp.Status)
		if status == 0 {
			status = http.StatusOK
		}
		if status < 100 || status > 599 {
			http.Error(w, "plugin returned invalid response status", http.StatusBadGateway)
			return
		}
		w.WriteHeader(status)
		if len(resp.Body) > 0 {
			w.Write(resp.Body)
		}
	}))

	// GET /_torana/ — embedded SPA dashboard.
	spaHandler := http.StripPrefix("/_torana/", controlplane.Handler())
	mux.Handle("/_torana/", s.controlPlaneGuard(spaHandler.ServeHTTP))

	// GET /_torana — redirect to /_torana/
	mux.HandleFunc("/_torana", s.controlPlaneGuard(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/_torana/", http.StatusMovedPermanently)
	}))

	// v1 is the canonical public control-plane API. The unversioned API remains
	// available for existing local scripts and the pre-v1 dashboard, but all v1
	// requests are translated before dispatch so both paths share exactly the
	// same auth, validation, and response behavior.
	mux.HandleFunc("/_torana/api/v1", s.controlPlaneGuard(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/_torana/api/v1/", http.StatusMovedPermanently)
	}))
	// Enabled plugins may contribute digest-bound, JSON-only agent operations
	// through agent.json. Dispatch still uses the existing isolated
	// run_on_http_request hook and env.serve_http approval.
	mux.HandleFunc("/_torana/api/v1/agent/plugins/", s.controlPlaneGuard(s.handlePluginAgentOperation))
	mux.HandleFunc("/_torana/api/v1/", s.controlPlaneGuard(func(w http.ResponseWriter, r *http.Request) {
		legacyPath := strings.TrimPrefix(r.URL.Path, "/_torana/api/v1")
		if legacyPath == "/" || legacyPath == "/agent" {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				writeAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed", "discovery only supports GET")
				return
			}
			writeAgentJSON(w, http.StatusOK, s.agentAPIDiscovery())
			return
		}
		if legacyPath == "/stats" {
			captured := newBufferedAgentResponse()
			statsHandler(captured, r)
			flushAgentResponse(w, captured)
			return
		}
		clone := r.Clone(r.Context())
		urlCopy := *r.URL
		clone.URL = &urlCopy
		clone.URL.Path = "/_torana/api" + legacyPath
		// Preserve streaming semantics for SSE. All bounded v1 responses pass
		// through a small adapter that turns legacy text errors into the stable
		// JSON error envelope advertised to agents.
		if legacyPath == "/stream" {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				writeAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed", "stream only supports GET")
				return
			}
			mux.ServeHTTP(w, clone)
			return
		}
		captured := newBufferedAgentResponse()
		mux.ServeHTTP(captured, clone)
		flushAgentResponse(w, captured)
	}))

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

		s.recordCompactionReports(rs)
		s.stats.RecordRequest(tr.bytesRead, tw.bytesWritten)
		s.stats.RecordTokens(int64(rs.UsageIn), int64(rs.UsageOut))
		s.stats.RecordCacheTokens(int64(rs.UsageCacheRead), int64(rs.UsageCacheWrite))
		// Record the conversation here rather than in the Rewrite hook: the
		// provider's cache token counts only exist once the response has been
		// read, and they are the ground truth for whether a prefix was warm.
		s.conversations.Observe(conversation.Observation{
			ID:             rs.ConversationID,
			CachePrefixKey: rs.CachePrefixKey,
			Provider:       rs.Provider,
			Model:          rs.Model,
			Format:         rs.InitialFormat,
			Path:           rs.Path,
			CacheRead:      rs.UsageCacheRead,
			CacheWrite:     rs.UsageCacheWrite,
		})
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
	// Background plugin ticks. Returns nil unless an operator configured an
	// interval; the loop additionally does nothing until some loaded plugin
	// both declares run_on_tick and holds env.background_tick.
	s.ticker = s.startTicker(cfg.Providers.Plugins.Runtime.TickInterval())
	return s, nil
}

// --- Lifecycle --------------------------------------------------------------

func (s *Server) controlPlaneGuard(next http.HandlerFunc) http.HandlerFunc {
	return s.controlPlaneGuardWithHeaders(next, false)
}

func (s *Server) controlPlanePluginGuard(next http.HandlerFunc) http.HandlerFunc {
	return s.controlPlaneGuardWithHeaders(next, true)
}

func (s *Server) controlPlaneGuardWithHeaders(next http.HandlerFunc, allowSameOriginFrame bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRemote(r.RemoteAddr) {
			http.Error(w, "control plane is localhost-only", http.StatusForbidden)
			return
		}
		if !isControlPlaneHost(r.Host) {
			http.Error(w, "invalid control-plane host", http.StatusForbidden)
			return
		}
		if isControlPlaneMutation(r.Method) && !isSameOriginControlPlaneRequest(r) {
			http.Error(w, "invalid control-plane origin", http.StatusForbidden)
			return
		}
		setControlPlaneSecurityHeaders(w, allowSameOriginFrame)
		next(w, r)
	}
}

func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// isControlPlaneHost defends a localhost-bound browser session from DNS
// rebinding. Only literal loopback hosts and localhost are accepted; external
// names that happen to resolve to 127.0.0.1 are intentionally rejected.
func isControlPlaneHost(hostport string) bool {
	if hostport == "" {
		return false
	}
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isControlPlaneMutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// Browsers attach Origin to fetch/XHR mutations. Requiring it to match the
// loopback Host prevents a malicious website from issuing state-changing
// requests to a developer's local proxy. Non-browser tools can use the
// explicit X-Torana-Local-Request header instead of forging browser metadata.
func isSameOriginControlPlaneRequest(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("X-Torana-Local-Request") == "1"
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	if !isControlPlaneHost(u.Host) {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func setControlPlaneSecurityHeaders(w http.ResponseWriter, allowSameOriginFrame bool) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	frameAncestors := "'none'"
	h.Set("X-Frame-Options", "DENY")
	if allowSameOriginFrame {
		frameAncestors = "'self'"
		h.Set("X-Frame-Options", "SAMEORIGIN")
	}
	// The embedded dashboard currently has an inline script and stylesheet, so
	// unsafe-inline is constrained to same-origin content rather than opening
	// the page to third-party script or frame sources.
	h.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors "+frameAncestors+"; form-action 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
}

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

func clonePluginConfig(src map[string]json.RawMessage) map[string]json.RawMessage {
	if src == nil {
		return make(map[string]json.RawMessage)
	}
	dst := make(map[string]json.RawMessage, len(src))
	for key, raw := range src {
		dst[key] = append(json.RawMessage(nil), raw...)
	}
	return dst
}

func pluginApprovals(src map[string]provider.PluginApproval) map[string]plugin.Approval {
	if src == nil {
		return nil
	}
	dst := make(map[string]plugin.Approval, len(src))
	for key, approval := range src {
		dst[key] = plugin.Approval{
			Digest:      approval.Digest,
			Permissions: append([]string(nil), approval.Permissions...),
			FailureMode: approval.FailureMode,
		}
	}
	return dst
}

func (s *Server) persistProviders(cfg provider.Config) error {
	path := s.configPath
	if path == "" {
		path = "config.json"
	}
	return provider.Save(path, cfg)
}

// applyProviderConfigTransaction keeps persisted state and in-memory state in
// lockstep. Candidate resources are brought up before persistence, and every
// live change is rolled back if a later step or the atomic save fails. Config
// file polling is intentionally not used: all live mutation comes through this
// coordinated path.
func (s *Server) applyProviderConfigTransaction(current, incoming provider.Config) error {
	if incoming.Port <= 0 || incoming.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}

	c1, c2 := incoming.Cache, current.Cache
	c1.Redis.Password, c2.Redis.Password = "", ""
	cacheChanged := c1 != c2
	mitmChanged := !reflect.DeepEqual(incoming.MITM, current.MITM)
	portChanged := incoming.Port != current.Port

	rollbackLive := func() {
		if portChanged {
			if err := s.SetPort(current.Port); err != nil {
				log.Printf("failed to roll back listener port: %v", err)
			}
		}
		if mitmChanged {
			if err := s.applyMITM(current.MITM); err != nil {
				log.Printf("failed to roll back MITM config: %v", err)
			}
		}
		if cacheChanged {
			if err := s.ReconfigureCache(current.Cache); err != nil {
				log.Printf("failed to roll back cache config: %v", err)
			}
		}
		s.SetProviders(current)
	}

	if cacheChanged {
		if err := s.ReconfigureCache(incoming.Cache); err != nil {
			return err
		}
	}
	if mitmChanged {
		if err := s.applyMITM(incoming.MITM); err != nil {
			if cacheChanged {
				_ = s.ReconfigureCache(current.Cache)
			}
			return err
		}
	}
	if portChanged {
		if err := s.SetPort(incoming.Port); err != nil {
			if mitmChanged {
				_ = s.applyMITM(current.MITM)
			}
			if cacheChanged {
				_ = s.ReconfigureCache(current.Cache)
			}
			return err
		}
	}
	if err := s.persistProviders(incoming); err != nil {
		rollbackLive()
		return fmt.Errorf("failed to persist config to disk: %w", err)
	}
	s.SetProviders(incoming)
	return nil
}

func (s *Server) resolveSecret(envName, encVal string) string {
	if envName != "" {
		if val := os.Getenv(envName); val != "" {
			return val
		}
	}
	if encVal != "" {
		if s.secrets == nil {
			log.Printf("failed to decrypt secret: store is not initialized")
			return ""
		}
		val, err := s.secrets.Decrypt(encVal)
		if err != nil {
			log.Printf("failed to decrypt secret: %v", err)
			return ""
		}
		return val
	}
	return ""
}

func (s *Server) normalizeSecretField(incomingEnc, storedEnc string) (string, error) {
	switch {
	case incomingEnc == secretSetSentinel:
		return storedEnc, nil
	case incomingEnc == "":
		return "", nil
	case secret.IsEncrypted(incomingEnc):
		return incomingEnc, nil
	default:
		if s.secrets == nil {
			return "", fmt.Errorf("secret store is not initialized")
		}
		return s.secrets.Encrypt(incomingEnc)
	}
}

// providerFieldsSent reports which keys the caller actually wrote for each
// provider, keyed by provider name. Presence is the whole point: encoding/json
// cannot distinguish "field omitted" from "field set to its zero value", and
// that distinction is what separates "the settings form doesn't manage pricing"
// from "the caller means to delete pricing". A malformed or unparseable body
// yields nil, which preserves nothing — the request will fail validation anyway.
func providerFieldsSent(body []byte) map[string]map[string]struct{} {
	var envelope struct {
		Providers map[string]map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil
	}
	sent := make(map[string]map[string]struct{}, len(envelope.Providers))
	for name, fields := range envelope.Providers {
		keys := make(map[string]struct{}, len(fields))
		for key := range fields {
			keys[key] = struct{}{}
		}
		sent[name] = keys
	}
	return sent
}

// unmanagedProviderFields are per-provider settings that no control-plane form
// currently renders. A client that rebuilds a provider object from a form would
// drop them, so they are carried forward unless explicitly written.
var unmanagedProviderFields = []string{"pricing", "responses_compaction", "cache"}

// preserveUnmanagedProviderFields copies unmanaged fields from the stored config
// into the incoming one wherever the caller left them out. It mutates incoming.
func preserveUnmanagedProviderFields(stored, incoming map[string]provider.Provider, sent map[string]map[string]struct{}) {
	for name, incP := range incoming {
		curP, existed := stored[name]
		if !existed {
			continue
		}
		for _, field := range unmanagedProviderFields {
			if _, written := sent[name][field]; written {
				continue
			}
			switch field {
			case "pricing":
				incP.Pricing = curP.Pricing
			case "responses_compaction":
				incP.ResponsesCompaction = curP.ResponsesCompaction
			case "cache":
				incP.Cache = curP.Cache
			}
		}
		incoming[name] = incP
	}
}

func redactConfigSecrets(cfg provider.Config) provider.Config {
	cfg.ControlPlane.Token = ""
	if cfg.Providers != nil {
		provs := make(map[string]provider.Provider, len(cfg.Providers))
		for name, p := range cfg.Providers {
			if p.APIKeyEnc != "" {
				p.APIKeyEnc = secretSetSentinel
			}
			provs[name] = p
		}
		cfg.Providers = provs
	}
	if cfg.Offload.APIKeyEnc != "" {
		cfg.Offload.APIKeyEnc = secretSetSentinel
	}
	if cfg.Cache.Redis.PasswordEnc != "" {
		cfg.Cache.Redis.PasswordEnc = secretSetSentinel
	}
	return cfg
}

// currentCache returns the active cache store under the guard mutex.
func (s *Server) currentCache() cache.Store {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	return s.sharedCache
}

// setCache atomically swaps the active cache store.
func (s *Server) setCache(c cache.Store) {
	s.cacheMu.Lock()
	s.sharedCache = c
	s.cacheMu.Unlock()
}

// newRuntime wires host callbacks for a WASM runtime.
func (s *Server) newRuntime() *wasm.Runtime {
	runtimeCfg := s.GetConfig().Providers.Plugins.Runtime
	memoryPages := uint32(0)
	if runtimeCfg.MemoryLimitMiB > 0 {
		if runtimeCfg.MemoryLimitMiB > 4096 {
			memoryPages = 65536
		} else {
			memoryPages = runtimeCfg.MemoryLimitMiB * 16
		}
	}
	rt := wasm.NewRuntimeWithCacheAndOptions(context.Background(), s.currentCache(), wasm.RuntimeOptions{
		PoolSize:         runtimeCfg.PoolSize,
		CallTimeout:      time.Duration(runtimeCfg.CallTimeoutMS) * time.Millisecond,
		MemoryLimitPages: memoryPages,
	})
	// Offload completion handler (cheap-model tool result
	// summarization), recording failures in /stats.
	rt.OffloadResultFunc = func(ctx context.Context, payloadJSON string) (economics.OffloadResult, error) {
		out, err := s.offloadCompletionResult(ctx, payloadJSON)
		if err != nil {
			log.Printf("[offload] %v", err)
			s.stats.RecordOffloadFailure()
		}
		return out, err
	}
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
	rt.CompactionReportFunc = func(ctx context.Context, pluginName string, report economics.CompactionReport) {
		rs := reqStateFrom(ctx)
		rs.CompactionReports = append(rs.CompactionReports, attributedCompactionReport{Plugin: pluginName, Report: report})
	}
	rt.EvaluateCompactionFunc = s.evaluateCompaction
	rt.RequestMutationFunc = func(ctx context.Context, requestPB []byte) {
		var request pb.ChatRequest
		if proto.Unmarshal(requestPB, &request) != nil {
			return
		}
		var meta map[string]any
		if json.Unmarshal(request.ToranaMetaJson, &meta) != nil {
			reqStateFrom(ctx).PendingRoute = nil
			return
		}
		raw, ok := meta["_route"]
		if !ok {
			reqStateFrom(ctx).PendingRoute = nil
			return
		}
		b, _ := json.Marshal(raw)
		var verdict routeVerdict
		if json.Unmarshal(b, &verdict) == nil {
			reqStateFrom(ctx).PendingRoute = &verdict
		}
	}
	// Plugins report compaction savings via torana_record_savings,
	// attributed per plugin in /stats and OTLP.
	rt.SavingsFunc = func(pluginName string, originalBytes, finalBytes int64) {
		s.stats.RecordCompaction(pluginName, originalBytes, finalBytes)
		metrics.RecordPluginSavings(context.Background(), pluginName, originalBytes-finalBytes)
	}
	rt.PluginCounterFunc = func(pluginName string, counter string, delta int64) {
		s.stats.RecordPluginCounter(pluginName, counter, delta)
	}
	// Durable plugin state (env.state_*). The plugin name is supplied by the
	// host from the calling module, never by the guest payload, so one plugin
	// cannot address another's namespace.
	if s.pluginState != nil {
		rt.StateGetFunc = s.pluginState.Get
		rt.StateSetFunc = s.pluginState.Set
		rt.StateKeysFunc = s.pluginState.Keys
	}
	rt.CachePricingFunc = s.cachePricing
	rt.SendRequestFunc = s.sendPluginRequest
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

// rebuildPipelineLocked swaps in a fresh pipeline (using the current
// s.sharedCache) and returns the displaced pipeline, undrained. Caller holds rebuildMu.
func (s *Server) rebuildPipelineLocked(pcfg provider.PluginsConfig) (*plugin.PluginPipeline, error) {
	rt := s.newRuntime()
	pp, err := plugin.NewPipeline(rt, plugin.PluginConfig{
		Dir:             pcfg.Dir,
		Order:           pcfg.Order,
		Config:          pcfg.Config,
		Approvals:       pluginApprovals(pcfg.Approvals),
		AllowUnapproved: pcfg.AllowUnapproved,
		Strict:          true,
	})
	if err != nil {
		rt.Close()
		return nil, err
	}

	old := s.pluginPipeline.Swap(pp)
	if old != nil {
		return old.(*plugin.PluginPipeline), nil
	}
	return nil, nil
}

// RebuildPipeline builds a fresh runtime + plugin pipeline using pcfg,
// then atomically swaps the active pipeline and drains the old one.
// If reloading fails (e.g. ordering constraint violation), returns the error
// without swapping the active pipeline.
func (s *Server) RebuildPipeline(pcfg provider.PluginsConfig) error {
	_, err := s.rebuildPipelineReportingSkips(pcfg)
	return err
}

// writePluginsWithWarnings emits the saved plugin configuration plus, when
// any enabled plugin was not loaded for want of an operator approval, a
// warnings block naming each one and how to resolve it. The save itself
// succeeds: unapproved plugins are skipped, never executed, so refusing the
// whole write would strand the operator — unrelated settings could not be
// changed while any stale entry sat in plugins.order.
func writePluginsWithWarnings(w http.ResponseWriter, plugins provider.PluginsConfig, skipped []plugin.SkippedPlugin) {
	if len(skipped) == 0 {
		b, _ := json.Marshal(plugins)
		w.Write(b)
		return
	}
	type warning struct {
		Name   string `json:"name"`
		Digest string `json:"digest"`
		Reason string `json:"reason"`
		Remedy string `json:"remedy"`
	}
	payload := struct {
		provider.PluginsConfig
		Warnings []warning `json:"warnings"`
	}{PluginsConfig: plugins}
	for _, sk := range skipped {
		payload.Warnings = append(payload.Warnings, warning{
			Name:   sk.Name,
			Digest: sk.Digest,
			Reason: sk.Reason,
			Remedy: "Open " + sk.Name + " in the control plane, review its requested capabilities, and approve this digest. Until then the plugin is enabled in configuration but not running.",
		})
	}
	b, _ := json.Marshal(payload)
	w.Write(b)
}

// rebuildPipelineReportingSkips is RebuildPipeline plus the list of enabled
// plugins that were not loaded for want of a digest-bound operator approval.
// A missing approval does not fail the rebuild: the plugin's code never runs,
// and the condition is reported to the caller so the control plane can tell
// the operator what to approve. Callers that only care about success can use
// RebuildPipeline.
func (s *Server) rebuildPipelineReportingSkips(pcfg provider.PluginsConfig) ([]plugin.SkippedPlugin, error) {
	s.rebuildMu.Lock()
	defer s.rebuildMu.Unlock()

	old, err := s.rebuildPipelineLocked(pcfg)
	if err != nil {
		return nil, err
	}
	if old != nil {
		go old.DrainAndClose()
	}
	var skipped []plugin.SkippedPlugin
	if pp, ok := s.pluginPipeline.Load().(*plugin.PluginPipeline); ok {
		skipped = pp.Skipped()
	}
	return skipped, nil
}

// ReconfigureCache rebuilds the shared cache store from newCache and atomically
// swaps the plugin pipeline to use the new store. The displaced pipeline and
// old store are drained and closed asynchronously after in-flight requests finish.
func (s *Server) ReconfigureCache(newCache cache.Config) error {
	s.rebuildMu.Lock()
	defer s.rebuildMu.Unlock()

	newCache.Redis.Password = s.resolveSecret(newCache.Redis.PasswordEnv, newCache.Redis.PasswordEnc)
	newStore, err := cache.New(newCache)
	if err != nil {
		return err
	}

	oldStore := s.currentCache()
	s.setCache(newStore)

	old, err := s.rebuildPipelineLocked(s.GetConfig().Providers.Plugins)
	if err != nil {
		s.setCache(oldStore)
		newStore.Close()
		return err
	}

	go func() {
		if old != nil {
			old.DrainAndClose()
		}
		if oldStore != nil {
			oldStore.Close()
		}
	}()

	s.configMu.Lock()
	s.config.Providers.Cache = newCache
	s.configMu.Unlock()

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

func (s *Server) setListener(ln net.Listener) {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	s.listener = ln
}

func (s *Server) swapListener(ln net.Listener) net.Listener {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	old := s.listener
	s.listener = ln
	return old
}

func (s *Server) currentPort() int {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if s.config.Providers.Port > 0 {
		return s.config.Providers.Port
	}
	if p, err := strconv.Atoi(s.config.Port); err == nil {
		return p
	}
	return 8080
}

// applyMITM (re)configures the MITM ingress to match cfg with no restart:
// it tears down any running ingress, then starts a fresh one if enabled.
func (s *Server) applyMITM(cfg provider.MITMConfig) error {
	s.mitmMu.Lock()
	defer s.mitmMu.Unlock()
	if !cfg.Enabled {
		if s.mitmSrv != nil {
			s.mitmSrv.Close() // stops the old CONNECT listener; frees the addr
			s.mitmSrv = nil
		}
		return nil
	}
	// Build (and validate) the new ingress BEFORE tearing down the old one. A
	// bad config (e.g. missing ca_dir) must not take down a running ingress —
	// otherwise a rejected settings PUT leaves the operator with no MITM at all.
	m, err := mitm.New(cfg, s.Handler())
	if err != nil {
		return err
	}
	// Only now that the new server is validated, stop the old one and free its
	// CONNECT addr so the new bind (which may reuse the same addr) can succeed.
	if s.mitmSrv != nil {
		s.mitmSrv.Close()
		s.mitmSrv = nil
	}
	go func() {
		if err := m.ListenAndServe(); err != nil {
			log.Printf("mitm ingress stopped: %v", err)
		}
	}()
	s.mitmSrv = m
	return nil
}

// Start binds the initial listener on bindHost:<current port> and serves it in
// the background. Non-blocking; returns the bind error only.
func (s *Server) Start(bindHost string) error {
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}
	s.listenerMu.Lock()
	s.bindHost = bindHost
	s.listenerMu.Unlock()
	ln, err := net.Listen("tcp", net.JoinHostPort(bindHost, strconv.Itoa(s.currentPort())))
	if err != nil {
		return err
	}
	s.setListener(ln)
	s.serveOnListener(ln)
	if err := s.applyMITM(s.config.Providers.MITM); err != nil {
		return fmt.Errorf("mitm: %w", err)
	}
	return nil
}

// serveOnListener runs httpServer.Serve(ln) in a goroutine. A listener closed
// for a port swap surfaces as a non-ErrServerClosed error here — that is
// expected and must NOT be fatal.
func (s *Server) serveOnListener(ln net.Listener) {
	go func() {
		if err := s.httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Printf("listener %s stopped: %v", ln.Addr(), err)
		}
	}()
}

// SetPort rebinds to newPort with no restart: bind the new listener, start
// serving it, then close the old listener (drains in-flight). On bind failure
// the old listener keeps serving and an error is returned.
func (s *Server) SetPort(newPort int) error {
	s.listenerMu.Lock()
	bindHost := s.bindHost
	s.listenerMu.Unlock()

	ln, err := net.Listen("tcp", net.JoinHostPort(bindHost, strconv.Itoa(newPort)))
	if err != nil {
		return err
	}
	s.serveOnListener(ln)
	old := s.swapListener(ln)
	if old != nil {
		old.Close()
	}
	// reflect the new port in config + the http.Server Addr
	s.configMu.Lock()
	s.config.Providers.Port = newPort
	s.config.Port = strconv.Itoa(newPort)
	if s.httpServer != nil {
		s.httpServer.Addr = ":" + strconv.Itoa(newPort)
	}
	s.configMu.Unlock()
	return nil
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
	s.setListener(ln)
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
	// Stop background ticks before the pipeline drains below: a tick in flight
	// holds the pipeline and may be mid-way through an outbound request, and
	// letting it outlive the runtime it is calling into is how a shutdown turns
	// into a crash.
	s.ticker.Close()
	s.mitmMu.Lock()
	if s.mitmSrv != nil {
		s.mitmSrv.Close()
		s.mitmSrv = nil
	}
	s.mitmMu.Unlock()
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
	s.conversations.Close()
	s.rebuildMu.Lock()
	s.cacheMu.Lock()
	if s.sharedCache != nil {
		s.sharedCache.Close()
		s.sharedCache = nil
	}
	s.cacheMu.Unlock()
	s.rebuildMu.Unlock()
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
