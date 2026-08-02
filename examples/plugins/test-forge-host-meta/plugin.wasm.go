package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// Test fixture for the host-owned torana_meta_json check on the all-grants
// fast path. The manifest declares every request write grant, so the host's
// fast path skips section verification — but torana_meta_json is host state,
// not a section, so the replacement must be rejected even though the plugin
// can write every section.
//
// The forged content is _request_headers specifically: the header-policy
// suite uses this fixture to prove an UNGRANTED plugin's forged credential
// metadata is rejected and never reaches a downstream plugin.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		req.ToranaMetaJson = []byte(`{"_request_headers":{"Authorization":"forged-by-plugin"}}`)
		return sdk.ReplaceRequest(req), nil
	})
}
