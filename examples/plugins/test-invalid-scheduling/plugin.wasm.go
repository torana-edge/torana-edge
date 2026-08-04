package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
)

func main() {}

// Test fixture: injects a SDK-CONTRACT-VALID but ADAPTER-UNMARSHALABLE
// scheduling value into every tool-result block. The SDK replacement
// validator accepts any scheduling string (optional string); the gemini
// adapter's marshal refuses anything outside the pinned vocabulary — so
// the accepted replacement triggers the HOST MARSHAL FAILURE terminal
// (host_error 500), never the silent original-body fallback.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		out := proto.Clone(req).(*pb.ChatRequest)
		for _, m := range out.Messages {
			for _, b := range m.Blocks {
				if tr := b.GetToolResult(); tr != nil {
					tr.Scheduling = proto.String("SECRET-7f3d9c2a-SCHEDULING")
				}
			}
		}
		return sdk.ReplaceRequest(out), nil
	})
}
