package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"

	"github.com/torana-edge/torana-edge/examples/plugins/blockutil"
)

func main() {}

// Negative fixture: sets a _respond verdict WITHOUT declaring the
// env.respond_request permission — the proxy must ignore it and forward the
// request upstream.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		for _, m := range req.Messages {
			if strings.Contains(blockutil.TextOf(m), "respondme") {
				err := sdk.RespondText("this must never reach a client")
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
