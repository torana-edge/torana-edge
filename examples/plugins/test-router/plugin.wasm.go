package main

import (
	"context"
	"strings"

	"github.com/torana-edge/torana-edge/examples/plugins/blockutil"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Example plugin exercising env.route_request (content-based routing):
//   - "routecheap"    → reroute to the "cheap" provider on model "small-model"
//   - "routemodel"    → model-only override on the original provider
//   - "routebroken"   → route to a provider that doesn't exist (must fail open)
//   - "routewrongfmt" → route to a provider with a different wire format
//     (must fail open — cross-format transcoding is unsupported)
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		for _, m := range req.Messages {
			switch {
			case strings.Contains(blockutil.TextOf(m), "routecheap"):
				// Route is a host-call SIDE EFFECT, so a route-only plugin
				// returns Pass. Returning ReplaceRequest here was the v1
				// "return the same request" footgun, and it hid a real bug:
				// compaction priced the original provider when a plugin
				// routed without replacing.
				sdk.RouteRequest("cheap", "small-model")
				return sdk.PassRequest(), nil
			case strings.Contains(blockutil.TextOf(m), "routemodel"):
				sdk.RouteRequest("", "tiny-model")
				return sdk.PassRequest(), nil
			case strings.Contains(blockutil.TextOf(m), "routebroken"):
				sdk.RouteRequest("no-such-provider", "small-model")
				return sdk.PassRequest(), nil
			case strings.Contains(blockutil.TextOf(m), "routewrongfmt"):
				sdk.RouteRequest("wrongfmt", "small-model")
				return sdk.PassRequest(), nil
			}
		}
		return sdk.PassRequest(), nil
	})
}
