package main

import (
	"context"
	"strings"

	"github.com/torana-edge/torana-edge/examples/plugins/blockutil"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Test fixture for trap semantics on the rejected-replacement path: it records
// verdicts and THEN returns a replacement that mutates a message without any
// write grant. Write-grant verification refuses the replacement, and the
// refused output must be treated like a trap — non-block verdicts recorded
// before it are discarded, while a recorded block fails closed and survives.
//
//   - "blockme"   → records a block AND a respond;
//   - "respondme" → records a respond only.
//
// Either way the guest then mutates message content (which it has no grant
// for) and returns the replacement.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		for _, m := range req.Messages {
			switch {
			case strings.Contains(blockutil.TextOf(m), "blockme"):
				sdk.BlockRequest(422, "blocked_then_invalid",
					"blocked before returning an invalid replacement")
				sdk.RespondRequest("this respond must be discarded")
			case strings.Contains(blockutil.TextOf(m), "respondme"):
				sdk.RespondRequest("this respond must be discarded")
			}
		}
		blockutil.SetText(req.Messages[0], blockutil.TextOf(req.Messages[0])+" [mutated without a write grant]")
		return sdk.ReplaceRequest(req), nil
	})
}
