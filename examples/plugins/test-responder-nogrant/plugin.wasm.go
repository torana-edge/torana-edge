package main

import (
	"context"
	"strings"

	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

// Negative fixture: sets a _respond verdict WITHOUT declaring the
// env.respond_request permission — the proxy must ignore it and forward the
// request upstream.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "respondme") {
				sdk.RespondRequest(req, "this must never reach a client")
				return req, nil
			}
		}
		return nil, nil
	})
}
