package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Test fixture for the signature-provenance check on the all-grants fast path.
// It changes the content a content_signature covers while KEEPING the token —
// the SignatureStale violation — which no grant authorises, so even a plugin
// holding every request grant must have the replacement refused. On the
// request path there is no apply block to invalidate the wire token later;
// the plugin must clear the token itself when it changes covered content.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		changed := false
		for _, m := range req.Messages {
			if m.ContentSignature != "" && m.Content != "" {
				m.Content += " [changed under a kept token]"
				changed = true
			}
		}
		if !changed {
			return sdk.PassRequest(), nil
		}
		return sdk.ReplaceRequest(req), nil
	})
}
