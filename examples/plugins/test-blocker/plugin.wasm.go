package main

import (
	"context"
	"strings"

	"github.com/torana-edge/torana-edge/examples/plugins/blockutil"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

// Example plugin exercising the env.block_request capability: if any message
// contains the word "blockme", the request is vetoed with a 422 error.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		for _, m := range req.Messages {
			if strings.Contains(blockutil.TextOf(m), "blockme") {
				sdk.BlockRequest(422, "blocked_by_test",
					"Blocked by test-blocker: request contained the trigger word.")
				return sdk.ReplaceRequest(req), nil
			}
		}
		return sdk.PassRequest(), nil
	})
}
