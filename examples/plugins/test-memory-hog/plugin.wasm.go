package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Allocates a large heap at init so INSTANTIATION (not compilation) requires
// 48 MiB of linear memory: it fits the default 64 MiB limit (the ABI
// inventory loads it) but deterministically fails newInstance under the
// lifecycle test's 16 MiB limit, proving the post-compile error path releases
// the compiled handle.
var _ = make([]byte, 48<<20)

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		return sdk.PassRequest(), nil
	})
}
