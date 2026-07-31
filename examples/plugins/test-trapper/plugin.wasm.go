package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Test fixture that always traps, declaring failure_mode "block".
//
// It exists to prove failure_mode is applied at the TRANSPORT boundary, not
// just inside the pipeline. Before that fix the proxy logged the pipeline's
// error and sent the request upstream anyway, so a security plugin whose
// manifest said "block" was fail-open on the real HTTP path while unit-level
// pipeline tests reported it blocked.
//
// A panic is the honest way to trap: it is what a real plugin's unhandled
// error does, and Migration A deliberately converts constructor and validation
// failures into traps so the host's failure_mode can act on them.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		panic("test-trapper always traps")
	})
}
