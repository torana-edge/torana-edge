package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Test fixture for the production extension-callback composition: the REAL
// proxy callbacks (egress, cache pricing, offload, record-savings), through
// the REAL dispatcher frame, decoded by the REAL SDK helpers — the path a
// production plugin runs, with none of it stubbed.
//
// The scenario is selected by the request model (the e2e posts
// {"model":"<scenario>",...}); the fixture runs that scenario's host calls
// and records what the guest observed into an observation struct. The model
// of the RETURNED request carries the observation JSON, so the e2e reads it
// from the captured upstream body — the fixture never needs its own channel.
//
// The observations are guest-side truth: what the SDK helper surfaced after
// the host framed its answer. That is the contract an author actually
// programs against.
type observation struct {
	Scenario string `json:"scenario"`

	// unbudgeted-send — the sdk.SendRequest helper path. The host has NO
	// egress budget for this plugin, so the refusal must surface as the
	// SDK's ErrEgressUnavailable sentinel with the stable not_configured
	// reason (the F5 composition proof).
	SendIsErrEgressUnavailable   bool   `json:"send_is_err_egress_unavailable,omitempty"`
	SendErrContainsNotConfigured bool   `json:"send_err_contains_not_configured,omitempty"`
	SendErrText                  string `json:"send_err_text,omitempty"`

	// Raw HostCallExtension outcomes (pricing-malformed, offload-*,
	// record-savings): the framed code when refused, 0 when accepted; the
	// value bytes when a value arm came back.
	RawCode  int32  `json:"raw_code,omitempty"`
	RawValue string `json:"raw_value,omitempty"`

	// GetCachePricing helper outcomes (pricing-unknown-provider,
	// pricing-unpriced-model): the domain value the SDK decoded.
	PricingStatus string `json:"pricing_status,omitempty"`
	PricingReason string `json:"pricing_reason,omitempty"`
}

// validReport is a CompactionReport the host's Normalize()/Valid() accept.
const validReport = `{"original_bytes":1000,"final_bytes":400,"estimated_tokens_removed":100,` +
	`"estimated_rewrite_span_tokens":5000,"expected_applications":1}`

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		obs := observation{Scenario: req.Model}
		switch req.Model {
		case "unbudgeted-send":
			// The SDK helper path: SendRequest → HostCallExtension →
			// dispatcher → real sendPluginRequest → authorize (no budget) →
			// framed NOT_CONFIGURED → SDK wraps ErrEgressUnavailable with the
			// not_configured reason.
			_, err := sdk.SendRequest(
				&pb.ChatRequest{Model: "gpt-x", Messages: []*pb.Message{{Role: "user", Content: "hi"}}},
				sdk.SendRequestOptions{Provider: "oai", Path: "/v1/chat/completions"},
			)
			obs.SendIsErrEgressUnavailable = errors.Is(err, sdk.ErrEgressUnavailable)
			obs.SendErrContainsNotConfigured = err != nil && strings.Contains(err.Error(), "not_configured")
			if err != nil {
				obs.SendErrText = err.Error()
			}

		case "pricing-malformed":
			// Raw HostCallExtension with malformed JSON: the host must frame
			// INVALID_ARGUMENT, never a status string in the value arm.
			_, herr, err := sdk.HostCallExtension("torana_cache_pricing", []byte("not json"))
			if err == nil && herr != nil {
				obs.RawCode = int32(herr.Code)
			}

		case "pricing-unknown-provider":
			// The GetCachePricing helper maps a framed NOT_CONFIGURED refusal
			// into the advisory shape: Status unavailable, Reason
			// not_configured.
			p, err := sdk.GetCachePricing("nope", "m")
			if err == nil {
				obs.PricingStatus = p.Status
				obs.PricingReason = p.Reason
			}

		case "pricing-unpriced-model":
			// A legitimate query result stays a domain value: unavailable with
			// no_pricing_configured, not a refusal.
			p, err := sdk.GetCachePricing("oai", "unpriced-model")
			if err == nil {
				obs.PricingStatus = p.Status
				obs.PricingReason = p.Reason
			}

		case "offload-disabled":
			// Offload not configured on this server: NOT_CONFIGURED, never
			// UNAVAILABLE — a plugin must not retry a permanently absent
			// feature as if it were a transient outage.
			v, herr, err := sdk.HostCallExtension("torana_offload_completion", []byte(`{"user_prompt":"u"}`))
			if err == nil && herr != nil {
				obs.RawCode = int32(herr.Code)
			} else if err == nil {
				obs.RawValue = string(v)
			}

		case "offload-bad-override":
			// A guest-selected api_key_env that does not match the provider's
			// host-owned configuration is a caller bug: INVALID_ARGUMENT.
			payload := `{"provider":"local","model":"local-1","user_prompt":"u",` +
				`"api_key_env":"GUEST_PICKED_SECRET"}`
			v, herr, err := sdk.HostCallExtension("torana_offload_completion", []byte(payload))
			if err == nil && herr != nil {
				obs.RawCode = int32(herr.Code)
			} else if err == nil {
				obs.RawValue = string(v)
			}

		case "offload-transport-dead":
			// A valid request to a dead endpoint: UNAVAILABLE.
			payload := `{"provider":"dead","model":"m","user_prompt":"u"}`
			v, herr, err := sdk.HostCallExtension("torana_offload_completion", []byte(payload))
			if err == nil && herr != nil {
				obs.RawCode = int32(herr.Code)
			} else if err == nil {
				obs.RawValue = string(v)
			}

		case "record-savings":
			// An acknowledgement: accepted with an EMPTY value arm and no
			// refusal (ruling 3 — no {"status":"ok"} ceremony).
			v, herr, err := sdk.HostCallExtension("torana_record_savings", []byte(validReport))
			if err == nil && herr != nil {
				obs.RawCode = int32(herr.Code)
			} else if err == nil {
				obs.RawValue = string(v)
			}
		}

		obsJSON, _ := json.Marshal(obs)
		req.Model = "fixture-obs:" + string(obsJSON)
		return sdk.ReplaceRequest(req), nil
	})
}
