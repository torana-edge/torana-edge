package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

func init() {
	sdk.OnBeforeRequest(func(context.Context, *pb.ChatRequest) (sdk.RequestResult, error) {
		sdk.MustSetIdentity("fixture-tenant")
		return sdk.PassRequest(), nil
	})
}
