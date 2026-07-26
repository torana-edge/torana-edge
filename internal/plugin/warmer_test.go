package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/pluginstate"
	"github.com/torana-edge/torana-edge/internal/wasm"
	"github.com/torana-edge/torana-plugin-sdk/pb"
)

// The warmer spends money on a timer, so these tests are less about it working
// and more about it refusing to work when it should. Every guard below is a
// case where sending a request would cost more than not sending one.

type sentRequest struct {
	Provider string
	Path     string
	Request  *pb.ChatRequest
}

// warmerHarness wires the host calls the warmer needs and records what it sent.
type warmerHarness struct {
	mu       sync.Mutex
	sent     []sentRequest
	pricing  string
	cacheHit bool // whether a refresh reports a cache read (alive) or write (lapsed)
	// httpStatus, when non-zero and not 200, simulates a provider that was
	// reached and refused the request — the 401 an unconfigured provider
	// credential produces.
	httpStatus int
}

func (h *warmerHarness) sentCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sent)
}

func (h *warmerHarness) lastSent() (sentRequest, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sent) == 0 {
		return sentRequest{}, false
	}
	return h.sent[len(h.sent)-1], true
}

func okPricing() string {
	return `{"status":"ok","cache_read_usd_per_mtok":0.3,"cache_write_usd_per_mtok":3.75,` +
		`"write_read_ratio":12.5,"break_even_refreshes":11,"refresh_on_read":true,` +
		`"shortest_ttl_seconds":300,"warm_interval_seconds":240}`
}

func newWarmerPipeline(t *testing.T, h *warmerHarness, conversations string, state *pluginstate.Store) *PluginPipeline {
	t.Helper()
	requireWASM(t, "../../plugins/cache_warmer/plugin.wasm")

	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { rt.Close() })

	if state == nil {
		var err error
		state, err = pluginstate.New(pluginstate.Options{})
		if err != nil {
			t.Fatalf("pluginstate.New: %v", err)
		}
	}
	rt.StateGetFunc = state.Get
	rt.StateSetFunc = state.Set
	rt.StateKeysFunc = state.Keys
	rt.CachePricingFunc = func(_ context.Context, _ string) string { return h.pricing }
	rt.SendRequestFunc = func(_ context.Context, _, payload string) string {
		var req struct {
			Provider  string `json:"provider"`
			RequestPB string `json:"request_pb"`
			Path      string `json:"path"`
		}
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return `{"status":"error","message":"bad payload"}`
		}
		raw, _ := base64.StdEncoding.DecodeString(req.RequestPB)
		var chat pb.ChatRequest
		_ = proto.Unmarshal(raw, &chat)

		h.mu.Lock()
		h.sent = append(h.sent, sentRequest{Provider: req.Provider, Path: req.Path, Request: &chat})
		h.mu.Unlock()

		// Reject what a real provider would reject. A harness that says "ok" to
		// anything cannot catch a malformed request, which is how an earlier
		// version of this plugin shipped a refresh that appended a second
		// consecutive user turn -- valid to this fake, rejected by Bedrock and
		// fragile on Anthropic.
		if why := providerWouldReject(&chat); why != "" {
			return `{"status":"error","message":"` + why + `"}`
		}

		if h.httpStatus != 0 && h.httpStatus != 200 {
			// The host reports transport success separately from what the
			// provider said, so a refused request still arrives as status "ok".
			return `{"status":"ok","http_status":` + strconv.Itoa(h.httpStatus) + `}`
		}
		if h.cacheHit {
			return `{"status":"ok","http_status":200,"usage":{"input":100,"output":1,"cache_read":95,"cache_write":0}}`
		}
		return `{"status":"ok","http_status":200,"usage":{"input":100,"output":1,"cache_read":0,"cache_write":100}}`
	}

	cfg := map[string]any{"conversations": conversations, "warm_for_minutes": 45}
	cfgJSON, _ := json.Marshal(cfg)

	pp, err := NewPipeline(rt, PluginConfig{
		Dir:             "../../plugins",
		Order:           []string{"cache_warmer"},
		AllowUnapproved: true,
		Config:          map[string]json.RawMessage{"cache_warmer": cfgJSON},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if pp.Len() != 1 {
		t.Fatalf("warmer not loaded (loaded=%d)", pp.Len())
	}
	return pp
}

// providerWouldReject applies the message-shape rules a provider enforces,
// returning why a request is invalid or "" when it is fine.
func providerWouldReject(req *pb.ChatRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}
	prev := ""
	for _, m := range req.Messages {
		// system may repeat; user and assistant may not. Anthropic documents
		// consecutive same-role turns as merged, but Bedrock rejects them and
		// real 400s are common enough that emitting them is not worth it.
		if (m.Role == "user" || m.Role == "assistant") && m.Role == prev {
			return "roles must alternate between user and assistant, but found multiple " + m.Role + " roles in a row"
		}
		prev = m.Role
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role == "assistant" && len(last.ToolCalls) > 0 {
		return "final assistant turn has tool calls with no tool result"
	}
	return ""
}

func warmerRequest(conversationID string) *engine.ChatRequest {
	return &engine.ChatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []engine.Message{
			{Role: engine.RoleSystem, Content: "You are a coding agent.",
				CacheControl: map[string]any{"type": "ephemeral"}},
			{Role: engine.RoleUser, Content: "refactor the loader"},
		},
		ToranaMeta: map[string]any{
			"_provider":        "anth",
			"_conversation_id": conversationID,
			"_path":            "/v1/messages",
		},
	}
}

// warmerRequestBreakpointOnUser is the shape a real coding harness sends: the
// cache breakpoint sits on the last user turn, so the cached prefix ENDS with a
// user message. Any refresh that appends another user turn produces two in a
// row, which Bedrock rejects outright and Anthropic only tolerates by merging.
func warmerRequestBreakpointOnUser(conversationID string) *engine.ChatRequest {
	return &engine.ChatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []engine.Message{
			{Role: engine.RoleSystem, Content: "You are a coding agent."},
			{Role: engine.RoleUser, Content: "refactor the loader"},
			{Role: engine.RoleAssistant, Content: "which file?"},
			{Role: engine.RoleUser, Content: "discovery.go",
				CacheControl: map[string]any{"type": "ephemeral"}},
		},
		ToranaMeta: map[string]any{
			"_provider": "anth", "_conversation_id": conversationID, "_path": "/v1/messages",
		},
	}
}

// tick fires one tick at now+offset. Times must be realistic: the plugin
// stores a deadline derived from the host's real clock during the request hook,
// so an arbitrary far-future tick trips the deadline rather than exercising the
// path under test.
func tick(t *testing.T, pp *PluginPipeline, id uint64, offset time.Duration) []TickOutcome {
	t.Helper()
	return pp.RunOnTick(context.Background(), id, &pb.TickRequest{
		TickId: id, UnixMillis: time.Now().Add(offset).UnixMilli(), IntervalMs: 240000,
	})
}

// TestWarmerRefreshesOptedInConversation is the happy path, and it checks the
// shape of what gets sent — the refresh must carry the cached prefix and ask
// for essentially no output, since the point is to touch the entry.
func TestWarmerRefreshesOptedInConversation(t *testing.T) {
	h := &warmerHarness{pricing: okPricing(), cacheHit: true}
	pp := newWarmerPipeline(t, h, "conv-a3f9", nil)

	if _, err := pp.RunBeforeRequest(context.Background(), 1, warmerRequest("conv-a3f9")); err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	// Past the 240s refresh interval, but inside the 45-minute deadline.
	tick(t, pp, 1, 5*time.Minute)

	if h.sentCount() != 1 {
		t.Fatalf("sent %d refreshes, want 1", h.sentCount())
	}
	got, _ := h.lastSent()
	if got.Provider != "anth" || got.Path != "/v1/messages" {
		t.Errorf("refresh went to %s%s, want anth/v1/messages", got.Provider, got.Path)
	}
	if got.Request.MaxTokens == nil || *got.Request.MaxTokens != 1 {
		t.Errorf("max_tokens = %v, want 1 — a refresh should not pay for output", got.Request.MaxTokens)
	}
	if len(got.Request.Messages) == 0 {
		t.Fatal("refresh carried no messages")
	}
	if got.Request.Messages[0].Content != "You are a coding agent." {
		t.Error("refresh did not carry the cached prefix, so it would not touch the entry")
	}
}

// TestWarmerRefreshIsAValidRequest is the regression test for a bug a reviewer
// caught: the refresh appended a trailing user turn on the theory that a
// request needs something after the cache breakpoint. It does not — a
// breakpoint on the final message is the ordinary shape — and appending one
// after a prefix that already ended with a user turn produced two in a row.
//
// The fixture matters as much as the assertion. With the breakpoint on the
// system message the appended turn still alternated, so the original test
// passed while the plugin was broken for every realistic conversation.
func TestWarmerRefreshIsAValidRequest(t *testing.T) {
	h := &warmerHarness{pricing: okPricing(), cacheHit: true}
	pp := newWarmerPipeline(t, h, "conv-a3f9", nil)

	if _, err := pp.RunBeforeRequest(context.Background(), 1, warmerRequestBreakpointOnUser("conv-a3f9")); err != nil {
		t.Fatal(err)
	}
	tick(t, pp, 1, 5*time.Minute)

	if h.sentCount() != 1 {
		t.Fatalf("sent %d refreshes, want 1", h.sentCount())
	}
	got, _ := h.lastSent()
	if why := providerWouldReject(got.Request); why != "" {
		t.Errorf("the refresh would be rejected by the provider: %s", why)
	}
	// And it must be the cached bytes, not something adjacent to them.
	if n := len(got.Request.Messages); n != 4 {
		t.Errorf("refresh carried %d messages, want the 4 of the cached prefix — "+
			"anything else is not the entry we are trying to touch", n)
	}
	if last := got.Request.Messages[len(got.Request.Messages)-1]; last.Content != "discovery.go" {
		t.Errorf("refresh ends on %q, want the prefix's own last message", last.Content)
	}
}

// TestWarmerSkipsPrefixEndingMidToolCall — a prefix ending on an unanswered
// tool call is not a request that can be sent at all, so warming should say so
// once rather than failing on every tick.
func TestWarmerSkipsPrefixEndingMidToolCall(t *testing.T) {
	h := &warmerHarness{pricing: okPricing(), cacheHit: true}
	pp := newWarmerPipeline(t, h, "conv-a3f9", nil)

	in := &engine.ChatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []engine.Message{
			{Role: engine.RoleSystem, Content: "You are a coding agent."},
			{Role: engine.RoleUser, Content: "read the file"},
			{Role: engine.RoleAssistant,
				ToolCalls:    []engine.ToolCall{{ID: "call_1", Name: "read"}},
				CacheControl: map[string]any{"type": "ephemeral"}},
		},
		ToranaMeta: map[string]any{
			"_provider": "anth", "_conversation_id": "conv-a3f9", "_path": "/v1/messages",
		},
	}
	if _, err := pp.RunBeforeRequest(context.Background(), 1, in); err != nil {
		t.Fatal(err)
	}
	outcomes := tick(t, pp, 1, 5*time.Minute)

	if h.sentCount() != 0 {
		t.Errorf("sent %d refreshes for a prefix that cannot be sent standalone", h.sentCount())
	}
	if len(outcomes) == 0 || !strings.Contains(outcomes[0].Note, "tool call") {
		t.Errorf("the refusal was not explained: %+v", outcomes)
	}
}

// TestWarmerWithoutClockGrantStoresNothing pins the behaviour when env.now is
// not granted.
//
// This regressed once already. When Now() returned a bare 0 on denial, the
// warmer stored a deadline 45 minutes after the epoch, so the very next tick
// found it long past and stopped every conversation reporting "deadline
// reached" — a plausible-sounding message for a completely different cause. The
// operator would see a plugin that looked like it was working.
//
// Fail closed is right; failing closed with a misleading reason is not.
func TestWarmerWithoutClockGrantStoresNothing(t *testing.T) {
	requireWASM(t, "../../plugins/cache_warmer/plugin.wasm")

	rt := wasm.NewRuntime(context.Background())
	defer rt.Close()
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rt.StateGetFunc, rt.StateSetFunc, rt.StateKeysFunc = state.Get, state.Set, state.Keys
	rt.CachePricingFunc = func(_ context.Context, _ string) string { return okPricing() }
	sent := 0
	rt.SendRequestFunc = func(_ context.Context, _, _ string) string {
		sent++
		return `{"status":"ok","http_status":200,"usage":{"cache_read":95}}`
	}

	digest, err := BundleDigestForDir("../../plugins/cache_warmer")
	if err != nil {
		t.Fatal(err)
	}
	pp, err := NewPipeline(rt, PluginConfig{
		Dir: "../../plugins", Order: []string{"cache_warmer"},
		Approvals: map[string]Approval{"torana/cache_warmer": {
			Digest: digest,
			// Everything the plugin asks for except env.now.
			Permissions: []string{
				"env.background_tick", "env.host_call.torana_send_request",
				"env.host_call.torana_cache_pricing", "env.state_get",
				"env.state_set", "env.state_keys", "env.plugin_config", "env.log",
			},
		}},
		Config: map[string]json.RawMessage{
			"cache_warmer": json.RawMessage(`{"conversations":"conv-a3f9","warm_for_minutes":45}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pp.RunBeforeRequest(context.Background(), 1, warmerRequestBreakpointOnUser("conv-a3f9")); err != nil {
		t.Fatal(err)
	}
	outcomes := tick(t, pp, 1, 5*time.Minute)

	if sent != 0 {
		t.Errorf("sent %d refreshes with no clock to schedule them by", sent)
	}
	if len(state.Keys("cache_warmer")) != 0 {
		v, _ := state.Get("cache_warmer", state.Keys("cache_warmer")[0])
		t.Errorf("stored an entry built from a zero clock: %s", v)
	}
	for _, o := range outcomes {
		if strings.Contains(o.Note, "deadline") {
			t.Errorf("reported %q, which blames the deadline for a missing env.now grant", o.Note)
		}
	}
}

// TestWarmerStopsOnProviderRefusal is the regression test for a bug found in
// audit: the host reports transport success separately from what the provider
// said, so a 401 arrived as status "ok" and the SDK did not error. The warmer
// counted every refused refresh as a completed one — burning its budget,
// letting the cache lapse anyway, and reporting warm actions to the operator.
//
// The likeliest cause of that 401 is the subtle part: on the normal request
// path Torana forwards the caller's credential, but a plugin-originated request
// has no caller, so a provider configured the ordinary way has no key at all.
func TestWarmerStopsOnProviderRefusal(t *testing.T) {
	h := &warmerHarness{pricing: okPricing(), cacheHit: true, httpStatus: 401}
	pp := newWarmerPipeline(t, h, "conv-a3f9", nil)

	if _, err := pp.RunBeforeRequest(context.Background(), 1, warmerRequestBreakpointOnUser("conv-a3f9")); err != nil {
		t.Fatal(err)
	}
	first := tick(t, pp, 1, 5*time.Minute)

	// One attempt is fine — it could not have known. Reporting it as an action
	// is not.
	for _, o := range first {
		if o.Actions > 0 {
			t.Errorf("a refused refresh was reported as %d completed actions", o.Actions)
		}
	}

	// And it must stop rather than pay the same 401 every tick forever.
	before := h.sentCount()
	for i := 2; i <= 5; i++ {
		tick(t, pp, uint64(i), time.Duration(i*5)*time.Minute)
	}
	if h.sentCount() != before {
		t.Errorf("kept retrying a refusing provider: %d sends after the first failure",
			h.sentCount()-before)
	}
}

// TestWarmerIgnoresUnlistedConversations — warming everything is how this
// feature loses money, so opt-in must actually gate.
func TestWarmerIgnoresUnlistedConversations(t *testing.T) {
	h := &warmerHarness{pricing: okPricing(), cacheHit: true}
	pp := newWarmerPipeline(t, h, "conv-other", nil)

	if _, err := pp.RunBeforeRequest(context.Background(), 1, warmerRequest("conv-a3f9")); err != nil {
		t.Fatal(err)
	}
	tick(t, pp, 1, 5*time.Minute)

	if h.sentCount() != 0 {
		t.Errorf("warmed an unlisted conversation %d times", h.sentCount())
	}
}

// TestWarmerStopsWhenCacheAlreadyExpired is the guard that keeps a lapsed
// conversation from becoming a standing charge. A cache WRITE means the refresh
// arrived too late and paid to rebuild — continuing would be paying to hold
// something the user may never return to.
func TestWarmerStopsWhenCacheAlreadyExpired(t *testing.T) {
	h := &warmerHarness{pricing: okPricing(), cacheHit: false} // reports a write
	pp := newWarmerPipeline(t, h, "conv-a3f9", nil)

	if _, err := pp.RunBeforeRequest(context.Background(), 1, warmerRequest("conv-a3f9")); err != nil {
		t.Fatal(err)
	}
	tick(t, pp, 1, 5*time.Minute)
	after := h.sentCount()
	if after != 1 {
		t.Fatalf("sent %d on the first tick, want 1", after)
	}

	// Several more ticks, each well past the interval.
	for i := 2; i <= 5; i++ {
		tick(t, pp, uint64(i), time.Duration(i*5)*time.Minute)
	}
	if h.sentCount() != after {
		t.Errorf("kept refreshing after the cache had expired: %d sends", h.sentCount())
	}
}

// TestWarmerStopsAtBreakEven is the arithmetic guard. Past (write/read - 1)
// refreshes, holding the entry open has cost more than the miss it avoids, and
// continuing diverges rather than settling.
func TestWarmerStopsAtBreakEven(t *testing.T) {
	// A ratio of 3 gives a break-even of 2 refreshes, so the third must refuse.
	h := &warmerHarness{cacheHit: true, pricing: `{"status":"ok","write_read_ratio":3,` +
		`"break_even_refreshes":2,"refresh_on_read":true,"shortest_ttl_seconds":300,` +
		`"warm_interval_seconds":240}`}
	pp := newWarmerPipeline(t, h, "conv-a3f9", nil)

	if _, err := pp.RunBeforeRequest(context.Background(), 1, warmerRequest("conv-a3f9")); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 6; i++ {
		tick(t, pp, uint64(i), time.Duration(i*5)*time.Minute)
	}

	if got := h.sentCount(); got != 2 {
		t.Errorf("sent %d refreshes, want exactly the 2 that pay for themselves", got)
	}
}

// TestWarmerDeclinesUnknownPricing — unknown economics is exactly when guessing
// is most expensive, so it must not send at all.
func TestWarmerDeclinesUnknownPricing(t *testing.T) {
	h := &warmerHarness{cacheHit: true, pricing: `{"status":"unavailable","reason":"no_pricing_configured"}`}
	pp := newWarmerPipeline(t, h, "conv-a3f9", nil)

	if _, err := pp.RunBeforeRequest(context.Background(), 1, warmerRequest("conv-a3f9")); err != nil {
		t.Fatal(err)
	}
	tick(t, pp, 1, 5*time.Minute)

	if h.sentCount() != 0 {
		t.Errorf("sent %d refreshes without pricing", h.sentCount())
	}
}

// TestWarmerDeclinesNonRefreshableCache is the OpenAI/DeepSeek case: automatic
// prefix caching has no lifetime the caller owns, so a refresh cannot keep
// anything alive and is pure cost.
func TestWarmerDeclinesNonRefreshableCache(t *testing.T) {
	h := &warmerHarness{cacheHit: true, pricing: `{"status":"ok","write_read_ratio":12.5,` +
		`"break_even_refreshes":11,"refresh_on_read":false,"shortest_ttl_seconds":300}`}
	pp := newWarmerPipeline(t, h, "conv-a3f9", nil)

	if _, err := pp.RunBeforeRequest(context.Background(), 1, warmerRequest("conv-a3f9")); err != nil {
		t.Fatal(err)
	}
	outcomes := tick(t, pp, 1, 5*time.Minute)

	if h.sentCount() != 0 {
		t.Errorf("sent %d refreshes to a cache that cannot be refreshed", h.sentCount())
	}
	// The operator should be told why nothing happened, not left guessing.
	if len(outcomes) == 0 || !strings.Contains(outcomes[0].Note, "cannot be refreshed") {
		t.Errorf("outcome did not explain the refusal: %+v", outcomes)
	}
}

// TestWarmerRespectsRefreshInterval — a refresh before the interval has elapsed
// is money spent to touch an entry that was not close to expiring.
func TestWarmerRespectsRefreshInterval(t *testing.T) {
	h := &warmerHarness{pricing: okPricing(), cacheHit: true}
	pp := newWarmerPipeline(t, h, "conv-a3f9", nil)

	if _, err := pp.RunBeforeRequest(context.Background(), 1, warmerRequest("conv-a3f9")); err != nil {
		t.Fatal(err)
	}
	tick(t, pp, 1, 5*time.Minute)
	if h.sentCount() != 1 {
		t.Fatalf("first refresh did not happen")
	}
	// Only 10 seconds after the first refresh, far short of the 240s interval.
	tick(t, pp, 2, 5*time.Minute+10*time.Second)
	if h.sentCount() != 1 {
		t.Errorf("refreshed again after 10s, ignoring the %ds interval", 240)
	}
}

// TestWarmerDoesNotMutateRequests is the determinism guard. This plugin
// observes and stores; if it ever wrote into a request it would change the very
// prefix it exists to preserve.
func TestWarmerDoesNotMutateRequests(t *testing.T) {
	h := &warmerHarness{pricing: okPricing(), cacheHit: true}
	pp := newWarmerPipeline(t, h, "conv-a3f9", nil)

	in := warmerRequest("conv-a3f9")
	before, err := json.Marshal(in.Messages)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pp.RunBeforeRequest(context.Background(), 1, in)
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(out.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("the warmer modified the request it was supposed to only observe:\nbefore %s\nafter  %s", before, after)
	}
}

// TestWarmerStateSurvivesRestart — the entry lives in durable state so a
// redeploy does not silently stop warming what the operator asked for.
func TestWarmerStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	state, err := pluginstate.New(pluginstate.Options{Path: dir + "/state.json"})
	if err != nil {
		t.Fatal(err)
	}

	h := &warmerHarness{pricing: okPricing(), cacheHit: true}
	first := newWarmerPipeline(t, h, "conv-a3f9", state)
	if _, err := first.RunBeforeRequest(context.Background(), 1, warmerRequest("conv-a3f9")); err != nil {
		t.Fatal(err)
	}

	// A fresh store reading the same file stands in for a restart.
	reloaded, err := pluginstate.New(pluginstate.Options{Path: dir + "/state.json"})
	if err != nil {
		t.Fatal(err)
	}
	h2 := &warmerHarness{pricing: okPricing(), cacheHit: true}
	second := newWarmerPipeline(t, h2, "conv-a3f9", reloaded)

	// No request this time — the tick must find the entry from disk alone.
	tick(t, second, 1, 5*time.Minute)
	if h2.sentCount() != 1 {
		t.Errorf("after a restart the warmer sent %d refreshes, want 1 — it lost track of what it was warming", h2.sentCount())
	}
}
