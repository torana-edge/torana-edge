package main

import (
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
	"github.com/torana-edge/torana-edge/pkg/pb"
)

func main() {}

func init() {
	sdk.OnChatRequest(func(req *pb.ChatRequest) (*pb.ChatRequest, error) {
		if req.Model == "" {
			req.Model = "claude-3-5-sonnet-20241022"
			return req, nil
		}
		return nil, nil
	})
}
