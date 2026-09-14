package main

import (
	"context"
	"errors"
	"fmt"
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
				err := sdk.BlockRequest(422, "blocked_by_test", "should be ignored — no grant")
				var refusal *sdk.HostCallRefusalError
				if !errors.As(err, &refusal) || refusal.Code != pb.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
					return sdk.PassRequest(), fmt.Errorf("expected permission refusal, got %v", err)
				}
				return sdk.ReplaceRequest(req), nil
			}
		}
		return sdk.PassRequest(), nil
	})
}
