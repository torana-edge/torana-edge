package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/provider"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// egressPayload builds what a plugin would pass to torana_send_request.
func egressPayload(t *testing.T, providerName, path string) string {
	t.Helper()
	chat := &engine.ChatRequest{
		Model:    "gpt-x",
		Messages: []engine.Message{{Role: engine.RoleUser, Content: "warm"}},
	}
	raw, err := proto.Marshal(pbconv.ToPBChatRequest(chat))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{
		"provider":   providerName,
		"request_pb": base64.StdEncoding.EncodeToString(raw),
		"path":       path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// egressServer stands a proxy in front of a fake upstream, with a budget for
// the named plugin.
func egressServer(t *testing.T, budget provider.EgressBudget) (*Server, *int) {
	t.Helper()
	calls := 0
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		if got := r.Header.Get("Authorization"); got != "Bearer sk-provider" {
			t.Errorf("upstream saw Authorization %q, want the provider's own key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"}}],
			"usage":{"prompt_tokens":100,"completion_tokens":2,
			         "prompt_tokens_details":{"cached_tokens":95}}}`)
	}))
	t.Cleanup(upstream.Close)

	t.Setenv("EGRESS_TEST_KEY", "sk-provider")

	cfg := provider.DefaultConfig()
	cfg.Providers = map[string]provider.Provider{
		"oai": {URL: upstream.URL, Format: "openai", APIKeyEnv: "EGRESS_TEST_KEY"},
	}
	cfg.Plugins.Runtime.Egress = map[string]provider.EgressBudget{"warmer": budget}

	srv, err := New(Config{
		Port:       "0",
		Providers:  cfg,
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { srv.conversations.Close() })
	return srv, &calls
}

// send invokes the host call the way the dispatcher does and decodes whichever
// arm the refusal or outcome landed on. A refusal is a framed classified
// HostError; a success is the domain envelope.
func send(t *testing.T, srv *Server, plugin, payload string) (egressResponse, *pb.HostError) {
	t.Helper()
	res := srv.sendPluginRequest(context.Background(), plugin, payload)
	if err := res.Validate(); err != nil {
		t.Fatalf("callback returned an invalid result: %v", err)
	}
	if res.Refusal() != nil {
		return egressResponse{}, res.Refusal()
	}
	var out egressResponse
	if err := json.Unmarshal(res.Value(), &out); err != nil {
		t.Fatalf("decode egress response: %v (body %q)", err, string(res.Value()))
	}
	return out, nil
}

// TestEgressSendsAndMeters is the happy path: the request reaches the provider,
// the provider's own credential is used, and usage comes back.
func TestEgressSendsAndMeters(t *testing.T) {
	srv, calls := egressServer(t, provider.EgressBudget{MaxCallsPerMinute: 10})

	got, herr := send(t, srv, "warmer", egressPayload(t, "oai", "/v1/chat/completions"))
	if herr != nil {
		t.Fatalf("a reached provider was framed as a refusal: %v", herr)
	}
	if got.HTTPStatus != 200 {
		t.Errorf("http_status = %d, want 200", got.HTTPStatus)
	}
	if *calls != 1 {
		t.Errorf("upstream saw %d calls, want 1", *calls)
	}
	if got.Usage == nil || got.Usage.CacheRead != 95 {
		t.Errorf("usage = %+v, want cache_read 95 — the signal that says whether a refresh worked", got.Usage)
	}
}

// TestEgressSuccessEnvelopeCarriesNoStatus — the constant success status field
// is gone (ruling 3): the error arm is the status channel, so a success
// envelope carries only the actual result fields. The SDK decodes an absent
// status as "" safely.
func TestEgressSuccessEnvelopeCarriesNoStatus(t *testing.T) {
	srv, _ := egressServer(t, provider.EgressBudget{MaxCallsPerMinute: 10})
	res := srv.sendPluginRequest(context.Background(), "warmer", egressPayload(t, "oai", "/v1/chat/completions"))
	if err := res.Validate(); err != nil {
		t.Fatalf("callback returned an invalid result: %v", err)
	}
	if res.Refusal() != nil {
		t.Fatalf("a reached provider was framed as a refusal: %v", res.Refusal())
	}
	if strings.Contains(string(res.Value()), `"status"`) {
		t.Errorf("success envelope still carries a status field: %s", res.Value())
	}
	var out egressResponse
	if err := json.Unmarshal(res.Value(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.HTTPStatus != 200 || out.Usage == nil || out.Usage.CacheRead != 95 {
		t.Errorf("result fields lost: %+v", out)
	}
}

// TestEgressRefusedWithoutBudget is the containment default. A capability that
// spends money must be unusable until an operator has said how much, or a
// plugin approved for some other reason inherits an open wallet.
//
// The refusal is framed NOT_CONFIGURED (never a status string in the envelope)
// so the SDK's ErrEgressUnavailable sentinel path is real, and the refusal is
// still metered.
func TestEgressRefusedWithoutBudget(t *testing.T) {
	srv, calls := egressServer(t, provider.EgressBudget{})

	_, herr := send(t, srv, "warmer", egressPayload(t, "oai", "/v1/chat/completions"))
	if herr == nil {
		t.Fatal("an unbudgeted plugin must not send")
	}
	if herr.Code != pb.ErrorCode_ERROR_CODE_NOT_CONFIGURED {
		t.Fatalf("code = %v, want NOT_CONFIGURED — the budget refusal is a configuration gap", herr.Code)
	}
	if !strings.Contains(herr.Message, "budget not configured") {
		t.Errorf("message does not explain how to fix it: %s", herr.Message)
	}
	if *calls != 0 {
		t.Errorf("upstream was reached %d times despite no budget", *calls)
	}
	if got := srv.stats.Snapshot().PluginCounters["warmer"]["egress_refused"]; got != 1 {
		t.Errorf("egress_refused counter = %d, want 1 — refusals must still be observability events", got)
	}
}

// TestEgressEnforcesCallRate — the budget must actually bind, not merely exist.
// An EXISTING rate budget whose rolling limit is exhausted is UNAVAILABLE:
// configured but unusable right now (the window rolls).
func TestEgressEnforcesCallRate(t *testing.T) {
	srv, calls := egressServer(t, provider.EgressBudget{MaxCallsPerMinute: 3})
	payload := egressPayload(t, "oai", "/v1/chat/completions")

	for i := 0; i < 3; i++ {
		if got, herr := send(t, srv, "warmer", payload); herr != nil || got.HTTPStatus != 200 {
			t.Fatalf("call %d refused early: %v", i+1, herr)
		}
	}
	_, herr := send(t, srv, "warmer", payload)
	if herr == nil || herr.Code != pb.ErrorCode_ERROR_CODE_UNAVAILABLE || !strings.Contains(herr.Message, "calls/minute") {
		t.Fatalf("the 4th call was not refused as UNAVAILABLE: %v", herr)
	}
	if *calls != 3 {
		t.Errorf("upstream saw %d calls, want exactly the budgeted 3", *calls)
	}
}

// TestEgressBudgetsArePerPlugin — one plugin exhausting its budget must not
// starve another.
func TestEgressBudgetsArePerPlugin(t *testing.T) {
	srv, _ := egressServer(t, provider.EgressBudget{MaxCallsPerMinute: 1})
	payload := egressPayload(t, "oai", "/v1/chat/completions")

	if got, herr := send(t, srv, "warmer", payload); herr != nil || got.HTTPStatus != 200 {
		t.Fatalf("first call refused: %v", herr)
	}
	if _, herr := send(t, srv, "warmer", payload); herr == nil {
		t.Fatal("warmer's budget did not bind")
	}
	// A different plugin has no budget at all, so it is refused for its own
	// reason rather than inheriting warmer's exhausted counter.
	_, herr := send(t, srv, "other", payload)
	if herr == nil || herr.Code != pb.ErrorCode_ERROR_CODE_NOT_CONFIGURED || !strings.Contains(herr.Message, "budget not configured") {
		t.Errorf("other plugin got warmer's error instead of its own: %v", herr)
	}
}

// TestEgressRejectsUnknownProvider is the containment boundary: a plugin can
// only reach endpoints the operator configured, so there is no attacker-supplied
// address and no SSRF surface.
func TestEgressRejectsUnknownProvider(t *testing.T) {
	srv, calls := egressServer(t, provider.EgressBudget{MaxCallsPerMinute: 10})

	_, herr := send(t, srv, "warmer", egressPayload(t, "http://169.254.169.254/latest/meta-data", "/"))
	if herr == nil {
		t.Fatal("a plugin reached a provider that is not configured")
	}
	if herr.Code != pb.ErrorCode_ERROR_CODE_NOT_CONFIGURED {
		t.Errorf("code = %v, want NOT_CONFIGURED — naming an unconfigured provider is a configuration gap", herr.Code)
	}
	if !strings.Contains(herr.Message, "unknown provider") {
		t.Errorf("unexpected message: %s", herr.Message)
	}
	if *calls != 0 {
		t.Error("upstream was reached for an unknown provider")
	}
}

// TestEgressRequiresPath — Torana forwards the caller's path rather than
// synthesizing one, so a plugin must supply it. Guessing would work for
// OpenAI-shaped providers and silently fail for Bedrock and Code Assist.
func TestEgressRequiresPath(t *testing.T) {
	srv, _ := egressServer(t, provider.EgressBudget{MaxCallsPerMinute: 10})

	_, herr := send(t, srv, "warmer", egressPayload(t, "oai", ""))
	if herr == nil || herr.Code != pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT || !strings.Contains(herr.Message, "path is required") {
		t.Errorf("a pathless request was not refused as INVALID_ARGUMENT: %v", herr)
	}
}

// TestEgressAppearsInFeed — work a plugin does outside any request must still
// be visible, or an operator cannot account for their own spend.
func TestEgressAppearsInFeed(t *testing.T) {
	srv, _ := egressServer(t, provider.EgressBudget{MaxCallsPerMinute: 10})
	if _, herr := send(t, srv, "warmer", egressPayload(t, "oai", "/v1/chat/completions")); herr != nil {
		t.Fatalf("send refused: %v", herr)
	}

	events := srv.feed.Snapshot()
	if len(events) == 0 {
		t.Fatal("a plugin-originated request left no trace in the feed")
	}
	ev := events[0]
	if ev.Verdict != "plugin-egress" {
		t.Errorf("verdict = %q, want plugin-egress", ev.Verdict)
	}
	if len(ev.Plugins) != 1 || ev.Plugins[0] != "warmer" {
		t.Errorf("plugins = %v, want the originating plugin named", ev.Plugins)
	}
	if ev.CacheReadTokens != 95 {
		t.Errorf("cache read tokens = %d, want 95", ev.CacheReadTokens)
	}
}

// TestEgressRejectsMalformedPayloads — the guest is untrusted input.
func TestEgressRejectsMalformedPayloads(t *testing.T) {
	srv, _ := egressServer(t, provider.EgressBudget{MaxCallsPerMinute: 10})

	for _, tc := range []struct{ name, payload, want string }{
		{"not json", `nonsense`, "invalid payload"},
		{"no provider", `{"request_pb":"","path":"/v1"}`, "provider is required"},
		{"bad base64", `{"provider":"oai","request_pb":"!!!","path":"/v1"}`, "not valid base64"},
		{"not a ChatRequest", `{"provider":"oai","request_pb":"ZGVmaW5pdGVseSBub3QgcHJvdG9idWY=","path":"/v1"}`, "ChatRequest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, herr := send(t, srv, "warmer", tc.payload)
			if herr == nil {
				t.Fatalf("malformed payload accepted: %+v", herr)
			}
			if herr.Code != pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
				t.Errorf("code = %v, want INVALID_ARGUMENT — a malformed payload is a caller bug, not a config gap", herr.Code)
			}
			if !strings.Contains(herr.Message, tc.want) {
				t.Errorf("message %q does not contain %q", herr.Message, tc.want)
			}
		})
	}
}

// TestEgressTokenBudget — the token ceiling is what stops a plugin whose calls
// are individually cheap but collectively enormous. An EXISTING token budget
// whose rolling limit is exhausted is UNAVAILABLE, not NOT_CONFIGURED.
//
// It is checked before each call against tokens already spent, because a call's
// cost is not knowable until the provider reports it. So the budget can
// overshoot by at most one call's worth, and it is a ceiling on what a plugin
// may *continue* spending rather than a hard cap on any single request.
func TestEgressTokenBudget(t *testing.T) {
	// Each response reports 102 tokens, so one call exhausts a 50-token budget.
	srv, calls := egressServer(t, provider.EgressBudget{MaxCallsPerMinute: 100, MaxTokensPerHour: 50})
	payload := egressPayload(t, "oai", "/v1/chat/completions")

	if got, herr := send(t, srv, "warmer", payload); herr != nil || got.HTTPStatus != 200 {
		t.Fatalf("first call refused: %v", herr)
	}
	_, herr := send(t, srv, "warmer", payload)
	if herr == nil || herr.Code != pb.ErrorCode_ERROR_CODE_UNAVAILABLE || !strings.Contains(herr.Message, "tokens/hour") {
		t.Fatalf("token budget did not bind as UNAVAILABLE: %v", herr)
	}
	if *calls != 1 {
		t.Errorf("upstream saw %d calls, want 1 before the token budget bound", *calls)
	}
}

// TestEgressMeterWindowsRoll — budgets are rolling windows, so a plugin
// throttled now must recover later rather than being locked out forever.
func TestEgressMeterWindowsRoll(t *testing.T) {
	m := newEgressMeter()
	now := time.Now()
	m.now = func() time.Time { return now }
	budget := provider.EgressBudget{MaxCallsPerMinute: 2}

	for i := 0; i < 2; i++ {
		if err := m.authorize("warmer", budget); err != nil {
			t.Fatalf("call %d refused early: %v", i+1, err)
		}
	}
	if err := m.authorize("warmer", budget); err == nil {
		t.Fatal("the third call inside one minute was allowed")
	}

	now = now.Add(61 * time.Second)
	if err := m.authorize("warmer", budget); err != nil {
		t.Errorf("the window did not roll: %v", err)
	}
}

// TestEgressRejectsUnrenderableRequest — a guest-supplied ChatRequest that the
// provider's format adapter cannot render is a CALLER bug, not a transient
// host outage: retrying the same request cannot help. A protobuf fixed64
// double field carries NaN bit-exactly, survives pbconv, and makes the
// adapter's json.Marshal fail — the guest-controlled vector this pins.
func TestEgressRejectsUnrenderableRequest(t *testing.T) {
	srv, calls := egressServer(t, provider.EgressBudget{MaxCallsPerMinute: 10})

	chat := &engine.ChatRequest{
		Model:       "gpt-x",
		Temperature: proto.Float64(math.NaN()),
		Messages:    []engine.Message{{Role: engine.RoleUser, Content: "warm"}},
	}
	raw, err := proto.Marshal(pbconv.ToPBChatRequest(chat))
	if err != nil {
		t.Fatalf("marshal ChatRequest: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"provider":   "oai",
		"request_pb": base64.StdEncoding.EncodeToString(raw),
		"path":       "/v1/chat/completions",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, herr := send(t, srv, "warmer", string(payload))
	if herr == nil {
		t.Fatal("an unrenderable request was sent upstream")
	}
	if herr.Code != pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Errorf("code = %v, want INVALID_ARGUMENT — the guest supplied an unrenderable request", herr.Code)
	}
	if !strings.Contains(herr.Message, "encode request") {
		t.Errorf("unexpected message: %s", herr.Message)
	}
	if *calls != 0 {
		t.Error("upstream was reached for an unrenderable request")
	}
}

// TestEgressRejectsInvalidPath — the path is guest-supplied (Torana forwards
// the caller's path rather than synthesizing one), so a URL that cannot be
// built is a caller bug, not a host outage.
func TestEgressRejectsInvalidPath(t *testing.T) {
	srv, calls := egressServer(t, provider.EgressBudget{MaxCallsPerMinute: 10})

	payload := egressPayload(t, "oai", "/v1/chat/completions\x00x")
	_, herr := send(t, srv, "warmer", payload)
	if herr == nil {
		t.Fatal("an invalid path was accepted")
	}
	if herr.Code != pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Errorf("code = %v, want INVALID_ARGUMENT — the guest supplied an invalid path", herr.Code)
	}
	if !strings.Contains(herr.Message, "build request") {
		t.Errorf("unexpected message: %s", herr.Message)
	}
	if *calls != 0 {
		t.Error("upstream was reached for an invalid path")
	}
}

// TestEgressSentinelsAreTyped — authorize classifies by typed sentinel, not by
// message text, so sendPluginRequest can map exhaustion to UNAVAILABLE without
// parsing prose. The unconfigured and exhausted states must be distinct.
func TestEgressSentinelsAreTyped(t *testing.T) {
	m := newEgressMeter()
	now := time.Now()
	m.now = func() time.Time { return now }

	if err := m.authorize("warmer", provider.EgressBudget{}); !errors.Is(err, ErrEgressBudgetNotConfigured) {
		t.Errorf("no budget: err = %v, want ErrEgressBudgetNotConfigured", err)
	}

	rate := provider.EgressBudget{MaxCallsPerMinute: 1}
	if err := m.authorize("warmer", rate); err != nil {
		t.Fatalf("first call refused: %v", err)
	}
	if err := m.authorize("warmer", rate); !errors.Is(err, ErrEgressRateExhausted) {
		t.Errorf("rate exhaustion: err = %v, want ErrEgressRateExhausted", err)
	}

	token := provider.EgressBudget{MaxCallsPerMinute: 100, MaxTokensPerHour: 1}
	m2 := newEgressMeter()
	now2 := time.Now()
	m2.now = func() time.Time { return now2 }
	m2.recordTokens("warmer", 5)
	if err := m2.authorize("warmer", token); !errors.Is(err, ErrEgressTokenExhausted) {
		t.Errorf("token exhaustion: err = %v, want ErrEgressTokenExhausted", err)
	}
}

// TestClassifyEgressRefusal pins the exhaustive budget-error mapping: no
// budget is NOT_CONFIGURED, both exhaustion states are UNAVAILABLE, and ANY
// OTHER error — including one this build has never seen — is INTERNAL, never
// silently collapsed into UNAVAILABLE.
func TestClassifyEgressRefusal(t *testing.T) {
	// A test-only sentinel stands in for a future budget error the switch has
	// no explicit case for.
	var unknownBudgetError = errors.New("some future budget error this build does not know")

	for _, tc := range []struct {
		name string
		err  error
		want pb.ErrorCode
	}{
		{"not configured", fmt.Errorf("wrapped: %w", ErrEgressBudgetNotConfigured), pb.ErrorCode_ERROR_CODE_NOT_CONFIGURED},
		{"rate exhausted", fmt.Errorf("wrapped: %w", ErrEgressRateExhausted), pb.ErrorCode_ERROR_CODE_UNAVAILABLE},
		{"token exhausted", fmt.Errorf("wrapped: %w", ErrEgressTokenExhausted), pb.ErrorCode_ERROR_CODE_UNAVAILABLE},
		{"unknown error", unknownBudgetError, pb.ErrorCode_ERROR_CODE_INTERNAL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyEgressRefusal(tc.err); got != tc.want {
				t.Errorf("classifyEgressRefusal(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestEgressInvalidRequestDoesNotConsumeBudget — the budget is the boundary
// for PROVIDER SPEND, not a caller-bug counter. With a one-call budget, an
// INVALID request (never reaching a provider) must consume nothing, so the
// next VALID request still has its slot.
func TestEgressInvalidRequestDoesNotConsumeBudget(t *testing.T) {
	srv, calls := egressServer(t, provider.EgressBudget{MaxCallsPerMinute: 1})

	// First an invalid request: bad base64 is refused before any transport.
	_, herr := send(t, srv, "warmer", `{"provider":"oai","request_pb":"!!!","path":"/v1/chat/completions"}`)
	if herr == nil || herr.Code != pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("invalid request was not refused as INVALID_ARGUMENT: %v", herr)
	}
	if *calls != 0 {
		t.Fatalf("invalid request reached upstream %d times", *calls)
	}

	// Then a valid request: the one-call budget slot must still be free.
	got, herr := send(t, srv, "warmer", egressPayload(t, "oai", "/v1/chat/completions"))
	if herr != nil {
		t.Fatalf("the valid request was refused after an invalid one: %v — the invalid attempt consumed the budget", herr)
	}
	if got.HTTPStatus != 200 {
		t.Errorf("http_status = %d, want 200", got.HTTPStatus)
	}
	if *calls != 1 {
		t.Errorf("upstream saw %d calls, want exactly the 1 valid one", *calls)
	}
}
