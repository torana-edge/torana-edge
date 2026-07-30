package main

import (
	"context"
	"strings"

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
			case strings.Contains(m.Content, "routecheap"):
				sdk.RouteRequest("cheap", "small-model")
				return sdk.ReplaceRequest(req), nil
			case strings.Contains(m.Content, "routemodel"):
				sdk.RouteRequest("", "tiny-model")
				return sdk.ReplaceRequest(req), nil
			case strings.Contains(m.Content, "routebroken"):
				sdk.RouteRequest("no-such-provider", "small-model")
				return sdk.ReplaceRequest(req), nil
			case strings.Contains(m.Content, "routewrongfmt"):
				sdk.RouteRequest("wrongfmt", "small-model")
				return sdk.ReplaceRequest(req), nil
			}
		}
		return sdk.PassRequest(), nil
	})
}
