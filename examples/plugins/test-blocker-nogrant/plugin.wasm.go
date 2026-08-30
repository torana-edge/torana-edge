package main

import (
	"context"
	"strings"

	"github.com/torana-edge/torana-edge/examples/plugins/blockutil"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

// Same as test-blocker but its manifest does NOT declare env.block_request, so
// the proxy must ignore the _block verdict and forward the request upstream.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		for _, m := range req.Messages {
			if strings.Contains(blockutil.TextOf(m), "blockme") {
				sdk.BlockRequest(422, "blocked_by_test", "should be ignored — no grant")
				return sdk.ReplaceRequest(req), nil
			}
		}
		return sdk.PassRequest(), nil
	})
}
