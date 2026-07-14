package main

import (
	"context"

	"github.com/torana-edge/torana-edge/pkg/pb"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.Log("torana_requests_total model="+req.Model, 1)
		return nil, nil
	})

	sdk.OnAfterResponse(func(ctx context.Context, resp *pb.ChatRequest) (*pb.ChatRequest, error) {
		sdk.Log("torana_responses_total model="+resp.Model, 1)
		return nil, nil
	})
}
