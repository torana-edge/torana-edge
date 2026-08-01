package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// TestExtensionContractE2E is the F5 composition proof: a real WASM fixture
// driven through the REAL server — real proxy callbacks (sendPluginRequest,
// cachePricing, offloadCompletionResult, record-savings dispatch), the real
// dispatcher frame, and the real SDK guest helpers. None of the path is
// stubbed, which is what makes the observation a production-contract truth
// rather than a harness truth.
//
// The fixture runs one scenario per request (selected by the request model)
// and records what the GUEST observed into the model of the returned request;
// the e2e reads it from the captured upstream body and asserts it.
func TestExtensionContractE2E(t *testing.T) {
	enabled := provider.OffloadConfig{Enabled: true, Provider: "cheap", Model: "cheap-1"}
	env := newContractServer(t, &enabled)

	for _, tc := range []struct {
		scenario string
		check    func(t *testing.T, obs contractObservation)
	}{
		{
			// The F5 proof: an unbudgeted send surfaces as the SDK's
			// ErrEgressUnavailable sentinel with the stable not_configured
			// reason — the exact contract the SDK's own tests pin, now proven
			// through the production composition.
			"unbudgeted-send",
			func(t *testing.T, obs contractObservation) {
				if !obs.SendIsErrEgressUnavailable {
					t.Errorf("errors.Is(err, sdk.ErrEgressUnavailable) = false — the guest saw %q", obs.SendErrText)
				}
				if !obs.SendErrContainsNotConfigured {
					t.Errorf("the refusal did not carry the not_configured reason: %q", obs.SendErrText)
				}
			},
		},
		{
			// Malformed pricing input must be framed INVALID_ARGUMENT — never a
			// status string smuggled through the value arm.
			"pricing-malformed",
			func(t *testing.T, obs contractObservation) {
				if obs.RawCode != int32(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT) {
					t.Errorf("pricing malformed: raw code = %d, want INVALID_ARGUMENT", obs.RawCode)
				}
			},
		},
		{
			// The GetCachePricing helper maps a framed NOT_CONFIGURED refusal
			// to the advisory unavailable/not_configured shape.
			"pricing-unknown-provider",
			func(t *testing.T, obs contractObservation) {
				if obs.PricingStatus != "unavailable" || obs.PricingReason != "not_configured" {
					t.Errorf("pricing unknown provider: status=%q reason=%q, want unavailable/not_configured",
						obs.PricingStatus, obs.PricingReason)
				}
			},
		},
		{
			// A legitimate query result stays a domain value.
			"pricing-unpriced-model",
			func(t *testing.T, obs contractObservation) {
				if obs.PricingStatus != "unavailable" || obs.PricingReason != "no_pricing_configured" {
					t.Errorf("pricing unpriced model: status=%q reason=%q, want unavailable/no_pricing_configured",
						obs.PricingStatus, obs.PricingReason)
				}
			},
		},
		{
			// A guest-selected api_key_env is a caller bug.
			"offload-bad-override",
			func(t *testing.T, obs contractObservation) {
				if obs.RawCode != int32(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT) {
					t.Errorf("offload bad override: raw code = %d, want INVALID_ARGUMENT", obs.RawCode)
				}
			},
		},
		{
			// A valid call to a dead endpoint is a transient outage.
			"offload-transport-dead",
			func(t *testing.T, obs contractObservation) {
				if obs.RawCode != int32(pb.ErrorCode_ERROR_CODE_UNAVAILABLE) {
					t.Errorf("offload dead endpoint: raw code = %d, want UNAVAILABLE", obs.RawCode)
				}
			},
		},
		{
			// record_savings is an acknowledgement: empty value arm, no refusal
			// (no {"status":"ok"} ceremony).
			"record-savings",
			func(t *testing.T, obs contractObservation) {
				if obs.RawCode != 0 || obs.RawValue != "" {
					t.Errorf("record_savings: code=%d value=%q, want empty success", obs.RawCode, obs.RawValue)
				}
			},
		},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			obs := env.post(t, tc.scenario)
			if obs.Scenario != tc.scenario {
				t.Errorf("fixture ran scenario %q, want %q", obs.Scenario, tc.scenario)
			}
			tc.check(t, obs)
		})
	}

	// Offload DISABLED needs a server whose offload is not configured: the
	// refusal is NOT_CONFIGURED (a permanently absent feature), never
	// UNAVAILABLE (which promises retries).
	t.Run("offload-disabled", func(t *testing.T) {
		env2 := newContractServer(t, nil)
		obs := env2.post(t, "offload-disabled")
		if obs.RawCode != int32(pb.ErrorCode_ERROR_CODE_NOT_CONFIGURED) {
			t.Errorf("offload disabled: raw code = %d, want NOT_CONFIGURED", obs.RawCode)
		}
	})
}

// contractObservation mirrors the fixture's observation struct (guest-side
// truth). Keep in sync with examples/plugins/test-extension-contract.
type contractObservation struct {
	Scenario string `json:"scenario"`

	SendIsErrEgressUnavailable   bool   `json:"send_is_err_egress_unavailable,omitempty"`
	SendErrContainsNotConfigured bool   `json:"send_err_contains_not_configured,omitempty"`
	SendErrText                  string `json:"send_err_text,omitempty"`

	RawCode  int32  `json:"raw_code,omitempty"`
	RawValue string `json:"raw_value,omitempty"`

	PricingStatus string `json:"pricing_status,omitempty"`
	PricingReason string `json:"pricing_reason,omitempty"`
}

// newContractServer stands up the REAL server with the test-extension-contract
// fixture loaded and NO egress budget for it (the unbudgeted-send scenario
// needs the refusal). offload configures the offload feature; nil disables it.
func newContractServer(t *testing.T, offload *provider.OffloadConfig) *contractEnv {
	t.Helper()
	requireWASM(t, fixturesDir+"/test-extension-contract/plugin.wasm")

	var mu sync.Mutex
	var lastBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastBody = append(lastBody[:0], b...)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"}}],`+
			`"usage":{"prompt_tokens":100,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":95}}}`)
	}))
	t.Cleanup(upstream.Close)

	cfg := provider.DefaultConfig()
	cfg.Providers = map[string]provider.Provider{
		// oai is the provider the fixture's send/pricing scenarios name.
		"oai":   {URL: upstream.URL, Format: "openai"},
		"cheap": {URL: upstream.URL, Format: "openai"},
		// local has NO api_key_env, so a guest-named api_key_env is a mismatch
		// (the bad-override scenario).
		"local": {URL: upstream.URL, Format: "openai"},
		// dead cannot be reached: the transport scenario.
		"dead": {URL: "http://127.0.0.1:1", Format: "openai"},
	}
	if offload != nil {
		cfg.Offload = *offload
	}
	cfg.Plugins.Dir = fixturesDir
	cfg.Plugins.Order = []string{"test-extension-contract"}
	cfg.Plugins.AllowUnapproved = true
	// Deliberately NO plugins.runtime.egress: the unbudgeted send must be
	// refused NOT_CONFIGURED.

	srv, err := New(Config{
		Port:       "0",
		Providers:  cfg,
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { srv.conversations.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	return &contractEnv{
		base:   "http://" + ln.Addr().String(),
		client: &http.Client{Timeout: 30 * time.Second},
		body:   func() []byte { mu.Lock(); defer mu.Unlock(); return append([]byte(nil), lastBody...) },
	}
}

type contractEnv struct {
	base   string
	client *http.Client
	body   func() []byte
}

// post drives one scenario through the real server and returns the
// observation the fixture embedded in the captured upstream request.
func (e *contractEnv) post(t *testing.T, scenario string) contractObservation {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"run"}]}`, scenario)
	resp, err := e.client.Post(e.base+"/provider/oai/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", scenario, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("scenario %s: upstream status %d", scenario, resp.StatusCode)
	}

	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(e.body(), &req); err != nil {
		t.Fatalf("captured upstream body does not decode: %v", err)
	}
	const prefix = "fixture-obs:"
	if !strings.HasPrefix(req.Model, prefix) {
		t.Fatalf("upstream saw model %q without a fixture observation", req.Model)
	}
	var obs contractObservation
	if err := json.Unmarshal([]byte(strings.TrimPrefix(req.Model, prefix)), &obs); err != nil {
		t.Fatalf("fixture observation does not decode: %v (model %q)", err, req.Model)
	}
	return obs
}
