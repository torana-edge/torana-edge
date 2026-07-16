package main

import (
	"context"
	"strings"

	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

// Same as test-blocker but its manifest does NOT declare env.block_request, so
// the proxy must ignore the _block verdict and forward the request upstream.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "blockme") {
				sdk.BlockRequest(req, 422, "blocked_by_test", "should be ignored — no grant")
				return req, nil
			}
		}
		return nil, nil
	})
}
