package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

// Marks the request model when it runs, so a test can prove it did NOT.
//
// Ordered after a blocking plugin, it must never be invoked: v1 handed a
// rejected request to every downstream plugin, which is how PII-laden payloads
// kept flowing to the compactor after the scanner refused them.
const marker = "+downstream-ran"

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		req.Model += marker
		return sdk.ReplaceRequest(req), nil
	})
}
