package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/torana-edge/torana-edge/internal/bridge"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
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
//   - Credentials are injected host-side from that provider's configured
//     Torana credential and are never visible to the guest. The caller's
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
	serial map[string]chan struct{} // cancellable token-budget admission, keyed by plugin
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
		serial: make(map[string]chan struct{}),
		now:    time.Now,
	}
}

// Typed egress-budget sentinels. authorize returns one of these (wrapped with
// the plugin-specific detail) so sendPluginRequest can classify by errors.Is
// instead of matching message text — and, more importantly, so the two
// exhaustion states stay distinct from the unconfigured state:
//
//   - no budget configured is NOT_CONFIGURED: the feature exists but the
//     operator has not said how much this plugin may spend;
//   - an EXISTING rate/token budget whose rolling limit is exhausted is
//     UNAVAILABLE: configured but unusable right now (the window will roll).
var (
	// ErrEgressBudgetNotConfigured is returned when no budget exists for the
	// plugin. The message keeps the operator-facing "budget not configured"
	// phrasing.
	ErrEgressBudgetNotConfigured = errors.New("egress budget not configured")
	// ErrEgressRateExhausted is returned when an existing call-rate budget is
	// exhausted. The message keeps the "calls/minute" phrasing.
	ErrEgressRateExhausted = errors.New("egress call-rate budget exhausted")
	// ErrEgressTokenExhausted is returned when an existing token budget is
	// exhausted. The message keeps the "tokens/hour" phrasing.
	ErrEgressTokenExhausted = errors.New("egress token budget exhausted")
)

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
		return fmt.Errorf("%w for plugin %q — set plugins.runtime.egress.%s.max_calls_per_minute",
			ErrEgressBudgetNotConfigured, plugin, plugin)
	}
	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls[plugin] = within(m.calls[plugin], now.Add(-time.Minute))
	if len(m.calls[plugin]) >= budget.MaxCallsPerMinute {
		return fmt.Errorf("%w: plugin %q has used its %d calls/minute budget",
			ErrEgressRateExhausted, plugin, budget.MaxCallsPerMinute)
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
			return fmt.Errorf("%w: plugin %q has used its %d tokens/hour budget",
				ErrEgressTokenExhausted, plugin, budget.MaxTokensPerHour)
		}
	} else {
		// Unlimited token budgets need no rolling token history.
		delete(m.tokens, plugin)
	}

	m.calls[plugin] = append(m.calls[plugin], now)
	return nil
}

// lockTokenBudget serializes admission, the network call, and post-response accounting when a
// token ceiling is enabled. Without this, many concurrent calls can all pass
// the same pre-spend check and collectively overshoot by many calls.
func (m *egressMeter) lockTokenBudget(ctx context.Context, plugin string, budget provider.EgressBudget) (func(), error) {
	if budget.MaxTokensPerHour <= 0 {
		return func() {}, ctx.Err()
	}
	m.mu.Lock()
	lock := m.serial[plugin]
	if lock == nil {
		lock = make(chan struct{}, 1)
		m.serial[plugin] = lock
	}
	m.mu.Unlock()
	select {
	case lock <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-lock
			return nil, err
		}
		return func() { <-lock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// classifyEgressRefusal maps an authorize failure to the ErrorCode a plugin
// can branch on, exhaustively: no budget is NOT_CONFIGURED (the capability
// exists but the operator has not sized it); an EXISTING exhausted budget
// (rate or token) is UNAVAILABLE (configured but unusable right now — the
// window rolls); ANY OTHER error is a host bug and must be INTERNAL, never
// silently collapsed into UNAVAILABLE.
func classifyEgressRefusal(err error) pb.ErrorCode {
	switch {
	case errors.Is(err, ErrEgressBudgetNotConfigured):
		return pb.ErrorCode_ERROR_CODE_NOT_CONFIGURED
	case errors.Is(err, ErrEgressRateExhausted), errors.Is(err, ErrEgressTokenExhausted):
		return pb.ErrorCode_ERROR_CODE_UNAVAILABLE
	default:
		return pb.ErrorCode_ERROR_CODE_INTERNAL
	}
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
	//
	// Pointer so an ABSENT key is distinguishable from a present zero-length
	// protobuf: an explicitly present all-default message encodes to zero
	// bytes and is a valid request, while a missing field is a caller bug.
	RequestPB *string `json:"request_pb"`
	// Path selects the provider's exposed client endpoint. Native providers
	// forward it; bridged providers validate it as the client contract and
	// synthesize their configured upstream endpoint.
	Path      string `json:"path,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

// egressResponse is the provider-outcome envelope: what the provider said
// (HTTP status, body, metering). There is deliberately NO message/status
// field — the error arm is the status channel, and a reached-but-refused
// provider is reported by its HTTPStatus.
type egressResponse struct {
	HTTPStatus int `json:"http_status,omitempty"`
	// Body is base64 in the provider's exposed client contract for the public
	// send_request host call. The private model-service path consumes a source
	// snapshot and never exposes this envelope to a guest.
	Body  string `json:"body,omitempty"`
	Usage *struct {
		Input      int64 `json:"input"`
		Output     int64 `json:"output"`
		CacheRead  int64 `json:"cache_read"`
		CacheWrite int64 `json:"cache_write"`
	} `json:"usage,omitempty"`
}

// sendPluginRequest answers the torana_send_request host call.
//
// Refusals are framed classified HostErrors: INVALID_ARGUMENT for malformed
// payloads, unrenderable guest requests, and invalid guest paths;
// NOT_CONFIGURED for unknown providers and missing budgets; UNAVAILABLE for
// exhausted budgets and transport failures; INTERNAL for a host-side envelope
// encode failure. The value arm carries provider outcomes only, and no longer
// carries a constant status field — the error arm is the status channel.
func (s *Server) sendPluginRequest(ctx context.Context, pluginName, payloadJSON string) wasm.ExtensionResult {
	return s.sendPluginRequestWithBudget(ctx, pluginName, payloadJSON, pluginEgressOptions{budgetKey: pluginName})
}

type pluginEgressOptions struct {
	boundBudget *provider.EgressBudget
	budgetKey   string
	// modelOverride pins an operator-bound model service to its resource
	// model. Nil lets an ordinary send_request use the provider bridge alias.
	modelOverride *string
	// sourceResult is an internal-only snapshot for model services. Guests get
	// the exposed client body; typed model results still need source-only facts
	// such as Anthropic stop_reason that have no Responses wire equivalent.
	sourceResult *pluginEgressSourceResult
}

type pluginEgressSourceResult struct {
	body       []byte
	formatName string
	set        bool
}

func (s *Server) sendPluginRequestWithBudget(ctx context.Context, pluginName, payloadJSON string, options pluginEgressOptions) wasm.ExtensionResult {
	var req egressRequest
	if err := json.Unmarshal([]byte(payloadJSON), &req); err != nil {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid payload: %v", err)
	}
	if req.Provider == "" {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "provider is required")
	}

	cfg := s.GetConfig().Providers
	prov, ok := cfg.Providers[req.Provider]
	if !ok {
		// Naming an unconfigured provider is the whole containment boundary:
		// a plugin can only reach endpoints the operator already trusts.
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "unknown provider %q", req.Provider)
	}

	if req.RequestPB == nil {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "request_pb is required")
	}
	raw, err := base64.StdEncoding.DecodeString(*req.RequestPB)
	if err != nil {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "request_pb is not valid base64: %v", err)
	}
	var pbReq pb.ChatRequest
	if err := proto.Unmarshal(raw, &pbReq); err != nil {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "request_pb is not a ChatRequest: %v", err)
	}
	// Guest-controlled request_pb must satisfy the current ABI replacement contract
	// (SDK ValidateReplacement) BEFORE conversion: empty required tool
	// arguments/schemas, nil nested values, and malformed JSON are refused
	// here — provider-side omissions normalize at the provider parse
	// boundary, never for a handwritten plugin guest.
	if err := pbReq.ValidateReplacement(); err != nil {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT,
			"request_pb does not satisfy the replacement contract: %v", err)
	}
	chat, err := pbconv.FromPBChatRequest(&pbReq)
	if err != nil {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "request_pb is not well-formed: %v", err)
	}
	if chat == nil {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "request_pb decoded to nothing")
	}
	// Proxy-internal metadata must not travel upstream, and a plugin has no
	// business setting it on an outbound request anyway.
	chat.ToranaMeta = engine.OptionalJSONObject{}
	path := req.Path
	if path == "" {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "path is required — Torana does not synthesize provider paths")
	}
	// The guest path contract: a root-relative request URI with no scheme, no
	// authority, and no userinfo. String concatenation is NOT safe here — a
	// path like "@attacker.example/v1" against a configured origin becomes
	// userinfo and redirects the request (and the provider credential) to
	// attacker.example. Parse both sides and prove the resolved target stays
	// on the configured origin before any credential or budget action.
	if !strings.HasPrefix(path, "/") {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "path must be root-relative, got %q", path)
	}
	base, err := url.Parse(prov.URL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "provider %q has an invalid URL %q", req.Provider, prov.URL)
	}
	u, err := url.ParseRequestURI(path)
	if err != nil {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid path %q: %v", path, err)
	}
	// A path beginning with "//" is a network-path reference in URI syntax
	// (ParseRequestURI folds it into the path); reject it outright so the
	// authority can never be ambiguous, along with absolute forms, opaque
	// forms, and userinfo.
	// ParseRequestURI folds a raw '#' into the path rather than parsing it as
	// a fragment, so reject fragments on the raw input; a real '#' in a path
	// must be %23-encoded.
	if strings.Contains(path, "#") || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || strings.HasPrefix(u.Path, "//") {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "path must stay on the configured provider origin, got %q", path)
	}
	targetCopy := *base
	targetCopy.Path = joinURLPath(base.Path, u.Path)
	targetCopy.RawPath = ""
	targetCopy.RawQuery = u.RawQuery
	targetCopy.Fragment = ""
	target := &targetCopy
	// Defense-in-depth invariant for future edits: today's copy only changes
	// path/query fields; the guest-authority rejection above is the primary guard.
	if target.Scheme != base.Scheme || target.Host != base.Host || target.Hostname() != base.Hostname() {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "path escapes the configured provider origin, got %q", path)
	}

	// A bridged provider exposes its client contract consistently to callers
	// and plugins. The guest path selects and validates that client contract;
	// Torana owns the upstream endpoint, body topology, and provider defaults.
	var clientProtocol bridge.Protocol
	var clientRequest *engine.ChatRequest
	f := format.Lookup(prov.Format)
	if f == nil || f.Request == nil {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "provider %q has no usable format adapter (%q)", req.Provider, prov.Format)
	}
	var body []byte
	if prov.Bridge != nil {
		clientProtocol = prov.Bridge.Client
		if !clientProtocol.MatchesPath(http.MethodPost, u.Path) {
			return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT,
				"path does not match provider %q client protocol", req.Provider)
		}
		if chat.Stream {
			return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT,
				"plugin egress does not support bridged streaming requests")
		}
		clientProtocol.ApplyTopology(chat)
		accepted, projectionErr := bridge.ProjectRequest(chat, clientProtocol, clientProtocol, bridge.RequestOptions{})
		if projectionErr != nil {
			return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT,
				"request_pb cannot represent the configured client protocol")
		}
		clientRequest = accepted
		candidate := *accepted
		if options.modelOverride != nil {
			candidate.Model = *options.modelOverride
		} else if prov.Bridge.Model != "" {
			candidate.Model = prov.Bridge.Model
		}
		chat, projectionErr = bridge.ProjectRequest(&candidate, clientProtocol, prov.Bridge.Upstream, bridgeOptions(prov))
		if projectionErr != nil {
			return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT,
				"request_pb cannot represent the configured upstream protocol")
		}
		if prov.Bridge.Upstream == bridge.OpenAIResponses {
			applyOpenAIResponsesCompaction(chat, prov)
		}
		body, err = bridge.MarshalRequest(prov.Bridge.Upstream, chat)
	} else {
		// OpenAIVariant is intentionally host-only and is not serialized in the
		// plugin ABI. Recover native wire topology from the validated path.
		if prov.Format == "openai" && strings.HasSuffix(u.Path, "/responses") {
			chat.OpenAIVariant = engine.OpenAIResponses
		}
		body, err = f.Request.Marshal(chat)
	}
	if err != nil {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "encode request for %s: %v", prov.Format, err)
	}

	if req.TimeoutMS < 0 {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "timeout_ms must not be negative")
	}
	timeout := defaultEgressTimeout
	if req.TimeoutMS > 0 {
		// Clamp BEFORE converting to a duration: a huge positive value can
		// overflow time.Duration and become negative, expiring locally while
		// still consuming a call slot.
		if req.TimeoutMS > int(maxEgressTimeout/time.Millisecond) {
			timeout = maxEgressTimeout
		} else {
			timeout = time.Duration(req.TimeoutMS) * time.Millisecond
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		// The path is guest-supplied (Torana forwards the caller's path rather
		// than synthesizing one), so a URL that cannot be built is a caller bug.
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "build request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept-Encoding", "identity")
	if prov.Bridge != nil {
		if err := prepareBridgeTransport(httpReq, prov, prov.Bridge.Upstream, chat); err != nil {
			return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_NOT_CONFIGURED,
				"provider %q bridge transport is unavailable", req.Provider)
		}
	}

	if prov.Auth.EffectiveMode() == "caller" {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "provider %q uses caller auth; plugin-originated calls require credential or none", req.Provider)
	}
	if err := applyProviderCredentialHeaders(callCtx, httpReq, prov, callerCredentials{}, s.resolveCredential); err != nil {
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "provider credential unavailable")
	}

	// Budget authorization happens AFTER all guest-input and request-build
	// validation and immediately BEFORE the network call: the budget is the
	// containment boundary for PROVIDER SPEND, not a caller-bug counter. A
	// malformed request that never reaches a provider consumes nothing; a
	// TRANSPORT ATTEMPT does — the slot was authorized, and a refused request
	// still needs the window to roll.
	budget := cfg.Plugins.Runtime.EgressBudgetFor(pluginName)
	if options.boundBudget != nil {
		budget = *options.boundBudget
	}
	unlockBudget, err := s.egress.lockTokenBudget(callCtx, options.budgetKey, budget)
	if err != nil {
		s.stats.RecordPluginCounter(pluginName, "egress_refused", 1)
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_UNAVAILABLE, "model call admission canceled: %v", err)
	}
	defer unlockBudget()
	if err := s.egress.authorize(options.budgetKey, budget); err != nil {
		s.stats.RecordPluginCounter(pluginName, "egress_refused", 1)
		return wasm.ExtensionRefusal(classifyEgressRefusal(err), "%v", err)
	}

	// The origin proof applies to the INITIAL request only; an http.Client
	// follows redirects by default, and Go strips Authorization but NOT
	// X-Api-Key on a cross-host redirect. redirectPolicy confines redirects to
	// the configured origin (any scheme/host change becomes the reached
	// provider outcome via http.ErrUseLastResponse, preserving the 3xx and the
	// fact that a transport attempt and budget slot occurred) and enforces the
	// ten-hop bound a custom CheckRedirect would otherwise remove.
	client := &http.Client{Timeout: timeout, CheckRedirect: redirectPolicy(base)}
	start := time.Now()
	resp, err := client.Do(httpReq)
	if err != nil {
		s.stats.RecordPluginCounter(pluginName, "egress_failed", 1)
		if errors.Is(err, errTooManyRedirects) {
			// A same-origin redirect loop is deterministic: retrying the same
			// request hits the same loop and spends again. NOT_CONFIGURED —
			// the operator must fix the configured provider endpoint.
			return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "provider %q redirected in a loop: %v", req.Provider, err)
		}
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_UNAVAILABLE, "request to %s failed: %v", req.Provider, err)
	}
	defer resp.Body.Close()

	// limit+1 read so an oversized body is DETECTED as an overflow rather than
	// silently truncated into a partial success: a truncated provider outcome
	// would meter and cache the wrong prefix.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxEgressResponseBytes+1))
	if err != nil {
		s.stats.RecordPluginCounter(pluginName, "egress_failed", 1)
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_UNAVAILABLE, "read response: %v", err)
	}
	if len(respBody) > maxEgressResponseBytes {
		// Deterministic, not transient: repeating the same request hits the
		// same limit and spends money again, so it is a caller-shape problem,
		// not a retryable outage (no dedicated resource-exhausted code exists
		// yet; INVALID_ARGUMENT is the closest class until one lands).
		s.stats.RecordPluginCounter(pluginName, "egress_failed", 1)
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "egress response exceeds %d bytes — reduce the request (e.g. max_tokens) or raise the limit", maxEgressResponseBytes)
	}
	if options.sourceResult != nil {
		options.sourceResult.body = append(options.sourceResult.body[:0], respBody...)
		options.sourceResult.formatName = f.Name
		options.sourceResult.set = true
	}

	// Usage and spend are always read in the upstream provider's native domain.
	// A translated client body may normalize token totals differently, but it
	// must never rewrite the budget fact after the provider has been called.
	var usage *engine.StreamUsage
	var decoded map[string]any
	if json.Unmarshal(respBody, &decoded) == nil {
		usage = extractResponse(f.Name, decoded).usage
	}
	if usage != nil {
		if tokens, reported := egressBillableTokens(f.Name, usage); budget.MaxTokensPerHour > 0 && reported {
			s.egress.recordTokens(options.budgetKey, tokens)
		}
	}

	clientBody := respBody
	// Model services consume the typed source snapshot above. Requiring an
	// otherwise unnecessary client-wire projection would reject source output
	// the model ABI can represent (for example Anthropic cache writes when the
	// exposed contract is Chat Completions). Public send_request remains strict:
	// its body always belongs to the exposed client contract.
	if clientProtocol.Valid() && options.sourceResult == nil {
		if resp.StatusCode >= http.StatusBadRequest {
			mapped := &http.Response{StatusCode: resp.StatusCode, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(respBody))}
			bridgeUpstreamError(callCtx, mapped, &bridgeExchange{Client: clientProtocol, Upstream: prov.Bridge.Upstream}, req.Provider)
			clientBody, _ = io.ReadAll(mapped.Body)
			_ = mapped.Body.Close()
		} else {
			clientBody, err = bridge.TranslateResponseWithRequest(prov.Bridge.Upstream, clientProtocol, respBody, chat.Model, clientRequest)
			if err != nil {
				s.stats.RecordPluginCounter(pluginName, "egress_calls", 1)
				s.stats.RecordPluginCounter(pluginName, "egress_failed", 1)
				s.recordEgressEvent(pluginName, req.Provider, chat.Model, resp.StatusCode, start, usage)
				return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_UNAVAILABLE,
					"provider returned a response that cannot be represented by its client protocol")
			}
		}
	}

	out := egressResponse{HTTPStatus: resp.StatusCode, Body: base64.StdEncoding.EncodeToString(clientBody)}

	// Usage is best-effort: a provider that reports none, or a body that is
	// not the JSON shape this format expects, simply means no metering data.
	if usage != nil {
		out.Usage = &struct {
			Input      int64 `json:"input"`
			Output     int64 `json:"output"`
			CacheRead  int64 `json:"cache_read"`
			CacheWrite int64 `json:"cache_write"`
		}{int64(usage.InputTokens), int64(usage.OutputTokens),
			int64(usage.CacheReadTokens), int64(usage.CacheWriteTokens)}
	}

	s.stats.RecordPluginCounter(pluginName, "egress_calls", 1)
	s.recordEgressEvent(pluginName, req.Provider, chat.Model, resp.StatusCode, start, usage)
	env, err := marshalEgress(out)
	if err != nil {
		// egressResponse is host-built (base64 body, ints); reaching here is a
		// host invariant, not a guest input.
		return wasm.ExtensionRefusal(pb.ErrorCode_ERROR_CODE_INTERNAL, "encode response: %v", err)
	}
	return wasm.ExtensionValue([]byte(env))
}

// validProviderUsage is shared by budget accounting and model-service results.
// Invalid counts are unknown usage in both paths: they must not exhaust a
// plugin's hourly budget while being withheld from the guest as untrustworthy.
func validProviderUsage(formatName string, usage *engine.StreamUsage) bool {
	if usage == nil {
		return false
	}
	for _, count := range []int{usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens} {
		if count < 0 || int64(count) > math.MaxInt32 {
			return false
		}
	}
	return formatName == "anthropic" || usage.CacheReadTokens <= usage.InputTokens
}

func egressBillableTokens(formatName string, usage *engine.StreamUsage) (int64, bool) {
	if !validProviderUsage(formatName, usage) {
		return 0, false
	}
	total := int64(usage.InputTokens) + int64(usage.OutputTokens)
	// Anthropic reports cache reads/creates separately from input_tokens.
	// Other formats use the provider's input total for token-budget accounting;
	// their cache-read counts are subsets and must not be counted twice.
	if formatName == "anthropic" {
		total += int64(usage.CacheReadTokens) + int64(usage.CacheWriteTokens)
	}
	return total, true
}

// recordEgressEvent puts plugin-originated traffic in the same feed as user
// traffic. Work a plugin does outside any request must still be visible, or an
// operator cannot tell where their spend went.
func (s *Server) recordEgressEvent(pluginName, providerName, model string, status int, start time.Time, usage *engine.StreamUsage) {
	ev := metrics.RequestEvent{
		Timestamp:      start.UTC().Format(time.RFC3339Nano),
		Provider:       providerName,
		RequestedModel: model,
		Status:         status,
		LatencyMS:      float64(time.Since(start).Microseconds()) / 1000,
		Verdict:        "plugin-egress",
		Plugins:        []string{pluginName},
	}
	if usage != nil {
		ev.TokensIn = int64(usage.InputTokens)
		ev.TokensOut = int64(usage.OutputTokens)
		ev.CacheReadTokens = int64(usage.CacheReadTokens)
		ev.CacheWriteTokens = int64(usage.CacheWriteTokens)
	}
	s.feed.Add(ev)
}

func marshalEgress(r egressResponse) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
