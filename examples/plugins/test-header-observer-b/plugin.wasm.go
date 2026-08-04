package main

import (
	"context"
	"encoding/json"
	"fmt"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"

	"github.com/torana-edge/torana-edge/examples/plugins/blockutil"
)

func main() {}

// Test fixture observing the per-plugin _request_headers projection on the
// chat surface. It reports what it saw by appending an assistant message
// (ir.message.write), so a test can assert the exact per-plugin projection:
// which credentials arrived, and whether the field was present at all.
//
// %s
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		// The cache read is the deterministic barrier for the snapshot test:
		// a wrapping store can mutate the caller's raw header map inside this
		// host call, and the NEXT plugin must still see the entry snapshot.
		_, _, _ = sdk.CacheGet("header-barrier")

		var meta map[string]any
		if len(req.ToranaMetaJson) > 0 {
			_ = json.Unmarshal(req.ToranaMetaJson, &meta)
		}
		hdrs, _ := meta["_request_headers"].(map[string]any)
		auth, _ := hdrs["Authorization"].(string)
		apikey, _ := hdrs["X-Api-Key"].(string)
		present := "false"
		if hdrs != nil {
			present = "true"
		}
		req.Messages = append(req.Messages, &pb.Message{
			Role:   "assistant",
			Blocks: blockutil.TextBlocks(fmt.Sprintf("observer auth=%s apikey=%s present=%s", auth, apikey, present)),
		})
		return sdk.ReplaceRequest(req), nil
	})
}
