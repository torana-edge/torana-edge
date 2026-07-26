package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

func send(t *testing.T, srv *Server, plugin, payload string) egressResponse {
	t.Helper()
	var out egressResponse
	if err := json.Unmarshal([]byte(srv.sendPluginRequest(context.Background(), plugin, payload)), &out); err != nil {
		t.Fatalf("decode egress response: %v", err)
	}
	return out
}

// TestEgressSendsAndMeters is the happy path: the request reaches the provider,
// the provider's own credential is used, and usage comes back.
func TestEgressSendsAndMeters(t *testing.T) {
	srv, calls := egressServer(t, provider.EgressBudget{MaxCallsPerMinute: 10})

	got := send(t, srv, "warmer", egressPayload(t, "oai", "/v1/chat/completions"))
	if got.Status != "ok" {
		t.Fatalf("status = %q: %s", got.Status, got.Message)
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

// TestEgressRefusedWithoutBudget is the containment default. A capability that
// spends money must be unusable until an operator has said how much, or a
// plugin approved for some other reason inherits an open wallet.
func TestEgressRefusedWithoutBudget(t *testing.T) {
	srv, calls := egressServer(t, provider.EgressBudget{})

	got := send(t, srv, "warmer", egressPayload(t, "oai", "/v1/chat/completions"))
	if got.Status != "error" {
		t.Fatalf("status = %q, want error — an unbudgeted plugin must not send", got.Status)
	}
	if !strings.Contains(got.Message, "budget not configured") {
		t.Errorf("message does not explain how to fix it: %s", got.Message)
	}
	if *calls != 0 {
		t.Errorf("upstream was reached %d times despite no budget", *calls)
	}
}

// TestEgressEnforcesCallRate — the budget must actually bind, not merely exist.
func TestEgressEnforcesCallRate(t *testing.T) {
	srv, calls := egressServer(t, provider.EgressBudget{MaxCallsPerMinute: 3})
	payload := egressPayload(t, "oai", "/v1/chat/completions")

	for i := 0; i < 3; i++ {
		if got := send(t, srv, "warmer", payload); got.Status != "ok" {
			t.Fatalf("call %d refused early: %s", i+1, got.Message)
		}
	}
	got := send(t, srv, "warmer", payload)
	if got.Status != "error" || !strings.Contains(got.Message, "calls/minute") {
		t.Fatalf("the 4th call was not refused: status=%q msg=%q", got.Status, got.Message)
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

	if got := send(t, srv, "warmer", payload); got.Status != "ok" {
		t.Fatalf("first call refused: %s", got.Message)
	}
	if got := send(t, srv, "warmer", payload); got.Status != "error" {
		t.Fatal("warmer's budget did not bind")
	}
	// A different plugin has no budget at all, so it is refused for its own
	// reason rather than inheriting warmer's exhausted counter.
	got := send(t, srv, "other", payload)
	if !strings.Contains(got.Message, "budget not configured") {
		t.Errorf("other plugin got warmer's error instead of its own: %s", got.Message)
	}
}

// TestEgressRejectsUnknownProvider is the containment boundary: a plugin can
// only reach endpoints the operator configured, so there is no attacker-supplied
// address and no SSRF surface.
func TestEgressRejectsUnknownProvider(t *testing.T) {
	srv, calls := egressServer(t, provider.EgressBudget{MaxCallsPerMinute: 10})

	got := send(t, srv, "warmer", egressPayload(t, "http://169.254.169.254/latest/meta-data", "/"))
	if got.Status != "error" {
		t.Fatal("a plugin reached a provider that is not configured")
	}
	if !strings.Contains(got.Message, "unknown provider") {
		t.Errorf("unexpected message: %s", got.Message)
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

	got := send(t, srv, "warmer", egressPayload(t, "oai", ""))
	if got.Status != "error" || !strings.Contains(got.Message, "path is required") {
		t.Errorf("a pathless request was not refused: status=%q msg=%q", got.Status, got.Message)
	}
}

// TestEgressAppearsInFeed — work a plugin does outside any request must still
// be visible, or an operator cannot account for their own spend.
func TestEgressAppearsInFeed(t *testing.T) {
	srv, _ := egressServer(t, provider.EgressBudget{MaxCallsPerMinute: 10})
	send(t, srv, "warmer", egressPayload(t, "oai", "/v1/chat/completions"))

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
			got := send(t, srv, "warmer", tc.payload)
			if got.Status != "error" {
				t.Fatalf("malformed payload accepted: %+v", got)
			}
			if !strings.Contains(got.Message, tc.want) {
				t.Errorf("message %q does not contain %q", got.Message, tc.want)
			}
		})
	}
}

// TestEgressTokenBudget — the token ceiling is what stops a plugin whose calls
// are individually cheap but collectively enormous.
//
// It is checked before each call against tokens already spent, because a call's
// cost is not knowable until the provider reports it. So the budget can
// overshoot by at most one call's worth, and it is a ceiling on what a plugin
// may *continue* spending rather than a hard cap on any single request.
func TestEgressTokenBudget(t *testing.T) {
	// Each response reports 102 tokens, so one call exhausts a 50-token budget.
	srv, calls := egressServer(t, provider.EgressBudget{MaxCallsPerMinute: 100, MaxTokensPerHour: 50})
	payload := egressPayload(t, "oai", "/v1/chat/completions")

	if got := send(t, srv, "warmer", payload); got.Status != "ok" {
		t.Fatalf("first call refused: %s", got.Message)
	}
	got := send(t, srv, "warmer", payload)
	if got.Status != "error" || !strings.Contains(got.Message, "tokens/hour") {
		t.Fatalf("token budget did not bind: status=%q msg=%q", got.Status, got.Message)
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
