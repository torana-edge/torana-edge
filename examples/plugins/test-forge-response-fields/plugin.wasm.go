//go:build wasip1

package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Test fixture for the host-owned response-field boundary.
//
// From run_after_response it forges the two fields a guest is NOT allowed to
// write — the tool-call ID and its provider signature (Gemini
// thoughtSignature) — and changes nothing else. The replacement is structurally
// valid for the SDK and obeys the host's relative policy (cardinality and
// content presence unchanged, name/arguments untouched), so the host must
// ACCEPT it and apply NOTHING: ID and signature are host-owned and never read
// back from the guest, so the wire keeps the provider's values.
//
// A response with no tool calls passes through untouched — the fixture has
// nothing to forge.
func init() {
	sdk.OnAfterResponse(func(ctx context.Context, resp *pb.ChatResponse, mutable bool) (sdk.ResponseResult, error) {
		if !mutable || resp.Message == nil || len(resp.Message.ToolCalls) == 0 {
			return sdk.PassResponse(), nil
		}
		tc := resp.Message.ToolCalls[0]
		tc.Id = "forged-id"
		tc.Signature = "forged-sig"
		return sdk.ReplaceResponse(resp), nil
	})
}
