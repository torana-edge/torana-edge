package main

import (
	"context"
	"strings"

	"github.com/torana-edge/torana-edge/sdk/pb"
	sdk "github.com/torana-edge/torana-edge/sdk/plugin-sdk"
)

func main() {}

// Example plugin exercising env.route_request (content-based routing):
//   - "routecheap"    → reroute to the "cheap" provider on model "small-model"
//   - "routemodel"    → model-only override on the original provider
//   - "routebroken"   → route to a provider that doesn't exist (must fail open)
//   - "routewrongfmt" → route to a provider with a different wire format
//     (must fail open — cross-format transcoding is unsupported)
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		for _, m := range req.Messages {
			switch {
			case strings.Contains(m.Content, "routecheap"):
				sdk.RouteRequest(req, "cheap", "small-model")
				return req, nil
			case strings.Contains(m.Content, "routemodel"):
				sdk.RouteRequest(req, "", "tiny-model")
				return req, nil
			case strings.Contains(m.Content, "routebroken"):
				sdk.RouteRequest(req, "no-such-provider", "small-model")
				return req, nil
			case strings.Contains(m.Content, "routewrongfmt"):
				sdk.RouteRequest(req, "wrongfmt", "small-model")
				return req, nil
			}
		}
		return nil, nil
	})
}
