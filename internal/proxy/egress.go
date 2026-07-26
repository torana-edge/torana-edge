package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-plugin-sdk/pb"
)

// torana_send_request lets a plugin originate a provider request.
//
// This is the largest capability in the plugin surface and the only one that
// spends money, so its limits are part of the capability rather than a later
// hardening pass:
//
//   - The destination is a CONFIGURED PROVIDER KEY, never a URL. A plugin
//     chooses among endpoints the operator already trusts; it cannot invent
//     one. There is no SSRF surface here because there is no attacker-supplied
//     address.
//   - Credentials are injected host-side from that provider's own
//     api_key_env/api_key_enc and are never visible to the guest. The caller's
//     credential is deliberately not reused: it is request-scoped, and a
//     background call has no caller to borrow from.
//   - Every call is metered against a per-plugin budget and recorded in the
//     request feed attributed to the plugin, so work a plugin does outside any
//     request is still visible to the operator.
//
// It does NOT route through failoverRoundTripper. That would consume the
// caller's rate-limit tokens and would need a synthesized RouteContext; a
// plugin-originated request is its own thing and should not borrow a user's
// budget.

const (
	// defaultEgressTimeout bounds one plugin-originated request.
	defaultEgressTimeout = 30 * time.Second
	// maxEgressTimeout caps what a plugin may ask for.
	maxEgressTimeout = 2 * time.Minute
	// maxEgressResponseBytes bounds what is read back into guest memory.
	maxEgressResponseBytes = 8 << 20
)

// egressMeter tracks per-plugin spend in rolling windows.
type egressMeter struct {
	mu     sync.Mutex
	calls  map[string][]time.Time // plugin → call timestamps in the last minute
	tokens map[string][]tokenSpend
	now    func() time.Time
}

type tokenSpend struct {
	at     time.Time
	tokens int64
}

func newEgressMeter() *egressMeter {
	return &egressMeter{
		calls:  make(map[string][]time.Time),
		tokens: make(map[string][]tokenSpend),
		now:    time.Now,
	}
}

// authorize reserves one call against the budget, reporting why not if refused.
//
// The token check compares against tokens ALREADY spent, because a call's cost
// is not knowable until the provider reports it. The budget therefore bounds
// what a plugin may continue spending rather than capping any single request,
// and can overshoot by at most one call's worth. The call-rate ceiling is the
// hard bound; the token ceiling is the backstop for calls that are individually
// cheap but collectively enormous.
func (m *egressMeter) authorize(plugin string, budget provider.EgressBudget) error {
	if budget.MaxCallsPerMinute <= 0 {
		return fmt.Errorf("egress budget not configured for plugin %q — set plugins.runtime.egress.%s.max_calls_per_minute", plugin, plugin)
	}
	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls[plugin] = within(m.calls[plugin], now.Add(-time.Minute))
	if len(m.calls[plugin]) >= budget.MaxCallsPerMinute {
		return fmt.Errorf("plugin %q has used its %d calls/minute budget", plugin, budget.MaxCallsPerMinute)
	}

	if budget.MaxTokensPerHour > 0 {
		spent := int64(0)
		kept := m.tokens[plugin][:0]
		cutoff := now.Add(-time.Hour)
		for _, s := range m.tokens[plugin] {
			if s.at.After(cutoff) {
				kept = append(kept, s)
				spent += s.tokens
			}
		}
		m.tokens[plugin] = kept
		if spent >= budget.MaxTokensPerHour {
			return fmt.Errorf("plugin %q has used its %d tokens/hour budget", plugin, budget.MaxTokensPerHour)
		}
	}

	m.calls[plugin] = append(m.calls[plugin], now)
	return nil
}

// recordTokens charges tokens actually spent, after the fact.
func (m *egressMeter) recordTokens(plugin string, tokens int64) {
	if tokens <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[plugin] = append(m.tokens[plugin], tokenSpend{at: m.now(), tokens: tokens})
}

func within(times []time.Time, cutoff time.Time) []time.Time {
	kept := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}

type egressRequest struct {
	Provider string `json:"provider"`
	// RequestPB is a base64-encoded pb.ChatRequest. Protobuf rather than JSON
	// because it is the same wire the hooks already speak, so a plugin can
	// forward a request it received without a lossy re-encoding.
	RequestPB string `json:"request_pb"`
	// Path overrides the upstream path. Torana never synthesizes a chat path —
	// it reuses whatever the caller sent — so a plugin replaying a conversation
	// must supply the path that conversation used.
	Path      string `json:"path,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

type egressResponse struct {
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Body       string `json:"body,omitempty"` // base64, provider-format
	Usage      *struct {
		Input      int64 `json:"input"`
		Output     int64 `json:"output"`
		CacheRead  int64 `json:"cache_read"`
		CacheWrite int64 `json:"cache_write"`
	} `json:"usage,omitempty"`
}

func egressError(format string, args ...any) string {
	b, _ := json.Marshal(egressResponse{Status: "error", Message: fmt.Sprintf(format, args...)})
	return string(b)
}

// sendPluginRequest answers the torana_send_request host call.
func (s *Server) sendPluginRequest(ctx context.Context, pluginName, payloadJSON string) string {
	var req egressRequest
	if err := json.Unmarshal([]byte(payloadJSON), &req); err != nil {
		return egressError("invalid payload: %v", err)
	}
	if req.Provider == "" {
		return egressError("provider is required")
	}

	cfg := s.GetConfig().Providers
	prov, ok := cfg.Providers[req.Provider]
	if !ok {
		// Naming an unconfigured provider is the whole containment boundary:
		// a plugin can only reach endpoints the operator already trusts.
		return egressError("unknown provider %q", req.Provider)
	}

	budget := cfg.Plugins.Runtime.EgressBudgetFor(pluginName)
	if err := s.egress.authorize(pluginName, budget); err != nil {
		s.stats.RecordPluginCounter(pluginName, "egress_refused", 1)
		return egressError("%v", err)
	}

	raw, err := base64.StdEncoding.DecodeString(req.RequestPB)
	if err != nil {
		return egressError("request_pb is not valid base64: %v", err)
	}
	var pbReq pb.ChatRequest
	if err := proto.Unmarshal(raw, &pbReq); err != nil {
		return egressError("request_pb is not a ChatRequest: %v", err)
	}
	chat := pbconv.FromPBChatRequest(&pbReq)
	if chat == nil {
		return egressError("request_pb decoded to nothing")
	}
	// Proxy-internal metadata must not travel upstream, and a plugin has no
	// business setting it on an outbound request anyway.
	chat.ToranaMeta = nil

	f := format.Lookup(prov.Format)
	if f == nil || f.Request == nil {
		return egressError("provider %q has no usable format adapter (%q)", req.Provider, prov.Format)
	}
	body, err := f.Request.Marshal(chat)
	if err != nil {
		return egressError("encode request for %s: %v", prov.Format, err)
	}

	path := req.Path
	if path == "" {
		return egressError("path is required — Torana does not synthesize provider paths")
	}

	timeout := defaultEgressTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
		if timeout > maxEgressTimeout {
			timeout = maxEgressTimeout
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := prov.URL + path
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return egressError("build request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept-Encoding", "identity")

	// Credentials come from the provider's own configuration. The caller's
	// credential is request-scoped and a plugin-originated call may have no
	// caller at all, so reusing it would work by accident during a request and
	// fail as a silent 401 on a tick.
	if key := s.resolveSecret(prov.APIKeyEnv, prov.APIKeyEnc); key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
		httpReq.Header.Set("X-Api-Key", key)
	}

	start := time.Now()
	resp, err := (&http.Client{Timeout: timeout}).Do(httpReq)
	if err != nil {
		s.stats.RecordPluginCounter(pluginName, "egress_failed", 1)
		return egressError("request to %s failed: %v", req.Provider, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxEgressResponseBytes))
	if err != nil {
		s.stats.RecordPluginCounter(pluginName, "egress_failed", 1)
		return egressError("read response: %v", err)
	}

	out := egressResponse{Status: "ok", HTTPStatus: resp.StatusCode, Body: base64.StdEncoding.EncodeToString(respBody)}

	// Usage is best-effort: a provider that reports none, or a body that is
	// not the JSON shape this format expects, simply means no metering data.
	var usage *engine.StreamUsage
	var decoded map[string]any
	if json.Unmarshal(respBody, &decoded) == nil {
		usage = extractResponse(f.Name, decoded).usage
	}
	if usage != nil {
		out.Usage = &struct {
			Input      int64 `json:"input"`
			Output     int64 `json:"output"`
			CacheRead  int64 `json:"cache_read"`
			CacheWrite int64 `json:"cache_write"`
		}{int64(usage.InputTokens), int64(usage.OutputTokens),
			int64(usage.CacheReadTokens), int64(usage.CacheWriteTokens)}
		s.egress.recordTokens(pluginName, int64(usage.InputTokens+usage.OutputTokens))
	}

	s.stats.RecordPluginCounter(pluginName, "egress_calls", 1)
	s.recordEgressEvent(pluginName, req.Provider, chat.Model, resp.StatusCode, start, usage)
	return marshalEgress(out)
}

// recordEgressEvent puts plugin-originated traffic in the same feed as user
// traffic. Work a plugin does outside any request must still be visible, or an
// operator cannot tell where their spend went.
func (s *Server) recordEgressEvent(pluginName, providerName, model string, status int, start time.Time, usage *engine.StreamUsage) {
	ev := metrics.RequestEvent{
		Timestamp: start.UTC().Format(time.RFC3339Nano),
		Provider:  providerName,
		Model:     model,
		Status:    status,
		LatencyMS: float64(time.Since(start).Microseconds()) / 1000,
		Verdict:   "plugin-egress",
		Plugins:   []string{pluginName},
	}
	if usage != nil {
		ev.TokensIn = int64(usage.InputTokens)
		ev.TokensOut = int64(usage.OutputTokens)
		ev.CacheReadTokens = int64(usage.CacheReadTokens)
		ev.CacheWriteTokens = int64(usage.CacheWriteTokens)
	}
	s.feed.Add(ev)
}

func marshalEgress(r egressResponse) string {
	b, err := json.Marshal(r)
	if err != nil {
		return egressError("encode response: %v", err)
	}
	return string(b)
}
