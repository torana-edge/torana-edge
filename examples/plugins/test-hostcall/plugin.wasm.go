package main

import (
	"context"
	"encoding/json"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Test fixture for the host-call surface a plugin uses to make decisions:
// durable state, the clock, cache pricing, and named counters.
//
// It exercises each one and records the outcome in its own durable state under
// a single key, so a test can read back exactly what the host answered without
// the fixture needing to mutate the request. That matters because these calls
// are how a plugin decides whether to act — a test that can see the answers can
// assert the host's side of the contract, including the denial path.
//
// Nothing here is written into the request. A host call's result is decision
// input; putting it in the prefix would make the output non-deterministic and
// invalidate the provider's cache every turn.
type observation struct {
	ClockNonZero    bool   `json:"clock_nonzero"`
	ClockErr        string `json:"clock_err,omitempty"`
	PricingStatus   string `json:"pricing_status,omitempty"`
	PricingErr      string `json:"pricing_err,omitempty"`
	StateRoundTrip  bool   `json:"state_round_trip"`
	StateErr        string `json:"state_err,omitempty"`
	CounterAccepted bool   `json:"counter_accepted"`
}

const observationKey = "last_observation"

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		var obs observation

		now, err := sdk.Now()
		obs.ClockNonZero = now > 0
		if err != nil {
			obs.ClockErr = err.Error()
		}

		meta := struct {
			Provider string `json:"_provider"`
		}{}
		_ = json.Unmarshal(req.ToranaMetaJson, &meta)
		if pricing, perr := sdk.GetCachePricing(meta.Provider, req.Model); perr != nil {
			obs.PricingErr = perr.Error()
		} else {
			obs.PricingStatus = pricing.Status
		}

		// Round-trip through durable state: write, read back, compare.
		//
		// Both channels are recorded. A refusal is a *HostError and a broken
		// boundary is an error, and a fixture that collapsed them would let a
		// test assert "state works" against a host that refused every call.
		if sherr, serr := sdk.StateSet("probe", "written"); serr != nil {
			obs.StateErr = serr.Error()
		} else if sherr != nil {
			obs.StateErr = sherr.Message
		} else if got, gherr, gerr := sdk.StateGet("probe"); gerr != nil {
			obs.StateErr = gerr.Error()
		} else if gherr != nil {
			obs.StateErr = gherr.Message
		} else {
			obs.StateRoundTrip = got == "written"
		}

		// A host FEATURE command, so it goes through HostCallExtension with an
		// opaque body. Acceptance is now the framed success arm rather than a
		// {"status":"ok"} string — v1's reply convention is gone.
		payload, _ := json.Marshal(map[string]any{"counter": "fixture_calls", "delta": 1})
		if _, cherr, cerr := sdk.HostCallExtension("torana_plugin_counter", payload); cerr == nil && cherr == nil {
			obs.CounterAccepted = true
		}

		_ = sdk.StateSetJSON(observationKey, obs)
		return sdk.PassRequest(), nil
	})
}
