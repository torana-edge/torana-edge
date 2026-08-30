package main

import (
	"context"
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
				sdk.RespondRequest("this must never reach a client")
				return sdk.ReplaceRequest(req), nil
			}
		}
		return sdk.PassRequest(), nil
	})
}
