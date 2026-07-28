package main

import (
	"context"
	"encoding/json"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/pb"
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
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
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
		if serr := sdk.StateSet("probe", "written"); serr != nil {
			obs.StateErr = serr.Error()
		} else if got, gerr := sdk.StateGet("probe"); gerr != nil {
			obs.StateErr = gerr.Error()
		} else {
			obs.StateRoundTrip = got == "written"
		}

		payload, _ := json.Marshal(map[string]any{"counter": "fixture_calls", "delta": 1})
		if res, cerr := sdk.HostCall("torana_plugin_counter", string(payload)); cerr == nil {
			obs.CounterAccepted = res == `{"status":"ok"}`
		}

		_ = sdk.StateSetJSON(observationKey, obs)
		return nil, nil
	})
}
