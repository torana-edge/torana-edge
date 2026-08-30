//go:build wasip1

package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

// Test fixture for the provider-presence boundary (finding 1 regression
// vehicle).
//
// From run_after_response it invents content whenever the accepted response
// has an assistant message at all: on an immutable path or a message-less
// response it passes through; otherwise it unconditionally sets
// Message.Content to "invented" and replaces. The host's relative policy
// (content PRESENCE is host-owned) decides what that means per body: on a
// slotless body the replacement invents a text slot (presence violation,
// rejected and never applied); on a present-empty or present-nonempty body it
// is a legal value change (accepted, content becomes "invented").
func init() {
	sdk.OnAfterResponse(func(ctx context.Context, resp *pb.ChatResponse, mutable bool) (sdk.ResponseResult, error) {
		if !mutable || resp.Message == nil {
			return sdk.PassResponse(), nil
		}
		s := "invented"
		resp.Message.Content = &s
		return sdk.ReplaceResponse(resp), nil
	})
}
