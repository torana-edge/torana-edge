package main

import (
	"context"
	"fmt"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Test fixture for the run_on_tick ABI.
//
// A tick has no request, so there is nowhere request-scoped to record that it
// happened. It writes to the shared cache instead, which survives between
// calls, and a later run_before_request reads it back — that round trip is how
// the e2e proves a tick actually fired rather than merely being scheduled.
func init() {
	sdk.OnTick(func(ctx context.Context, tick *pb.TickRequest) (sdk.TickResult, error) {
		// Echo the whole TickRequest back so tests can assert the host
		// populated every field, not just that something arrived.
		_, _ = sdk.CacheSet("last_tick", fmt.Sprintf("id=%d millis=%d interval=%d",
			tick.TickId, tick.UnixMillis, tick.IntervalMs))

		// Tick 1 reports nothing to do, so the idle path is exercised by a real
		// guest rather than only in unit tests. v2 spells it TickIdle: v1
		// needed a `handled` flag because an all-defaults TickResult marshals
		// to zero bytes.
		if tick.TickId == 1 {
			return sdk.TickIdle(), nil
		}
		return sdk.TickDid(int32(tick.TickId), fmt.Sprintf("tick %d", tick.TickId)), nil
	})

	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		v, herr, err := sdk.CacheGet("last_tick")
		if err != nil || herr != nil || v == "" {
			// A miss means no tick has fired yet, which is the state this
			// fixture exists to observe changing.
			return sdk.PassRequest(), nil
		}
		req.Model = req.Model + "+" + v
		return sdk.ReplaceRequest(req), nil
	})
}
