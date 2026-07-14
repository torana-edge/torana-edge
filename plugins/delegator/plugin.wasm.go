package main

import (
	"context"

	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		if req.Model == "" {
			req.Model = "claude-3-5-sonnet-20241022"
			return req, nil
		}
		return nil, nil
	})
}
