package main

import (
	"context"
	"strings"

	"github.com/torana-edge/torana-edge/sdk/pb"
	sdk "github.com/torana-edge/torana-edge/sdk/plugin-sdk"
)

func main() {}

// Example plugin exercising the env.block_request capability: if any message
// contains the word "blockme", the request is vetoed with a 422 error.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "blockme") {
				sdk.BlockRequest(req, 422, "blocked_by_test",
					"Blocked by test-blocker: request contained the trigger word.")
				return req, nil
			}
		}
		return nil, nil
	})
}
