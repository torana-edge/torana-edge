package main

import (
	"context"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/pb"
)

func main() {}

// Example plugin exercising env.respond_request: if any message contains the
// word "respondme", the plugin serves a canned completion directly — the
// upstream provider is never called.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "respondme") {
				sdk.RespondRequest(req, "canned response from test-responder")
				return req, nil
			}
		}
		return nil, nil
	})
}
