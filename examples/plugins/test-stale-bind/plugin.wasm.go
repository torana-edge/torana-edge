package main

import (
	"context"

	"github.com/torana-edge/torana-edge/examples/plugins/blockutil"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func hasContentSignature(m *pb.Message) bool {
	for _, b := range m.Blocks {
		if t := b.GetText(); t != nil && t.Signature != "" {
			return true
		}
	}
	return false
}

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
			if hasContentSignature(m) && blockutil.TextOf(m) != "" {
				blockutil.SetText(m, blockutil.TextOf(m)+" [changed under a kept token]")
				changed = true
			}
		}
		if !changed {
			return sdk.PassRequest(), nil
		}
		return sdk.ReplaceRequest(req), nil
	})
}
