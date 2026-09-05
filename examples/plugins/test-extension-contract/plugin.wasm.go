package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/torana-edge/torana-edge/examples/plugins/blockutil"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

// Test fixture for the production extension-callback composition: the REAL
// proxy callbacks (egress, cache pricing, record-savings), through
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
	// egress budget for this plugin, so the refusal must surface as a
	// classified *HostCallRefusalError with code NOT_CONFIGURED (the F5
	// composition proof).
	SendRefusalCode int32  `json:"send_refusal_code,omitempty"`
	SendErrText     string `json:"send_err_text,omitempty"`

	// Raw HostCallExtension outcomes (pricing-malformed,
	// record-savings). RawArm is the explicit discriminator — "value",
	// "refusal", or "goerror" — so a call that failed before producing an
	// arm can never be mistaken for a zero-valued observation. RawSucceeded
	// is true only for the value arm; RawGoError carries a Go error's text;
	// RawCode is the framed code when refused; RawValue the value bytes.
	// RawValuePresent is the arm's own presence bit: the ABI's empty success
	// decodes as a non-nil zero-length slice, distinct from "no value arm".
	RawSucceeded    bool   `json:"raw_succeeded,omitempty"`
	RawArm          string `json:"raw_arm,omitempty"`
	RawGoError      string `json:"raw_go_error,omitempty"`
	RawValuePresent bool   `json:"raw_value_present,omitempty"`
	RawCode         int32  `json:"raw_code,omitempty"`
	RawValue        string `json:"raw_value,omitempty"`

	// GetCachePricing helper outcomes (pricing-unknown-provider,
	// pricing-unpriced-model): the domain value the SDK decoded.
	PricingStatus string `json:"pricing_status,omitempty"`
	PricingReason string `json:"pricing_reason,omitempty"`
}

// validReport is a current CompactionReport the host accepts.
const validReport = `{"original_bytes":1000,"final_bytes":400,"estimated_tokens_removed":100,` +
	`"estimated_rewrite_span_tokens":5000,"expected_applications":1,"source":"transformation",` +
	`"pricing_resource":"target"}`

// recordRaw captures the three-way outcome of a raw HostCallExtension call:
// a Go error (the call failed before the host produced an arm), a framed
// refusal, or a value arm. Recording WHICH arm arrived — plus the arm's
// presence bit — is what makes a later assertion unable to read "the call
// failed" as a zero-valued success.
func recordRaw(obs *observation, v []byte, herr *pb.HostError, err error) {
	switch {
	case err != nil:
		obs.RawArm = "goerror"
		obs.RawGoError = err.Error()
	case herr != nil:
		obs.RawArm = "refusal"
		obs.RawCode = int32(herr.Code)
	default:
		obs.RawArm = "value"
		obs.RawSucceeded = true
		obs.RawValuePresent = v != nil
		obs.RawValue = string(v)
	}
}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		obs := observation{Scenario: req.Model}
		switch req.Model {
		case "unbudgeted-send":
			// The SDK helper path: SendRequest → HostCallExtension →
			// dispatcher → real sendPluginRequest → authorize (no budget) →
			// framed NOT_CONFIGURED → classified *HostCallRefusalError.
			_, err := sdk.SendRequest(
				&pb.ChatRequest{Model: "gpt-x", Messages: []*pb.Message{{Role: "user", Blocks: blockutil.TextBlocks("hi")}}},
				sdk.SendRequestOptions{Provider: "oai", Path: "/v1/chat/completions"},
			)
			var refusal *sdk.HostCallRefusalError
			if errors.As(err, &refusal) {
				obs.SendRefusalCode = int32(refusal.Code)
			}
			if err != nil {
				obs.SendErrText = err.Error()
			}

		case "pricing-malformed":
			// Raw HostCallExtension with malformed JSON: the host must frame
			// INVALID_ARGUMENT, never a status string in the value arm.
			v, herr, err := sdk.HostCallExtension("torana_cache_pricing", []byte("not json"))
			recordRaw(&obs, v, herr, err)

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

		case "record-savings":
			// An acknowledgement: accepted with an EMPTY value arm and no
			// refusal (ruling 3 — no {"status":"ok"} ceremony). The
			// discriminator records the value arm as PRESENT and zero-length,
			// so a transport/protocol failure (which would record a goerror)
			// can never be read as this ack.
			v, herr, err := sdk.HostCallExtension("torana_record_savings", []byte(validReport))
			recordRaw(&obs, v, herr, err)
		}

		obsJSON, _ := json.Marshal(obs)
		req.Model = "fixture-obs:" + string(obsJSON)
		return sdk.ReplaceRequest(req), nil
	})
}
