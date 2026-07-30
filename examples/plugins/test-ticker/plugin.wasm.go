package main

import (
	"context"
	"encoding/json"
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
	sdk.OnTick(func(ctx context.Context, tick *pb.TickRequest) (*pb.TickResult, error) {
		// Echo the whole TickRequest back so tests can assert the host
		// populated every field, not just that something arrived.
		args, _ := json.Marshal(map[string]any{
			"key": "last_tick",
			"value": fmt.Sprintf("id=%d millis=%d interval=%d",
				tick.TickId, tick.UnixMillis, tick.IntervalMs),
		})
		_, _ = sdk.HostCall("env.cache_set", string(args))

		// Tick 1 reports nothing to do, so the "nil means did nothing" path is
		// exercised by a real guest rather than only in unit tests.
		if tick.TickId == 1 {
			return sdk.PassRequest(), nil
		}
		return &pb.TickResult{
			Handled: true,
			Actions: int32(tick.TickId),
			Note:    fmt.Sprintf("tick %d", tick.TickId),
		}, nil
	})

	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		v, err := sdk.HostCall("env.cache_get", "last_tick")
		if err != nil || v == "" {
			return sdk.PassRequest(), nil
		}
		req.Model = req.Model + "+" + v
		return sdk.ReplaceRequest(req), nil
	})
}
