package main

import (
	"context"
	"strings"

	"github.com/torana-edge/torana-edge/examples/plugins/blockutil"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
)

func main() {}

// Test fixture: appends a CONTRACT-VALID redacted_thinking assistant
// message when any message contains "redactme". The openai chat and
// bedrock adapters refuse redacted thinking at marshal — so the accepted
// replacement triggers the HOST MARSHAL FAILURE terminal (host_error
// 500) for both formats; without the marker the request passes through.
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		poison := false
		for _, m := range req.Messages {
			if strings.Contains(blockutil.TextOf(m), "redactme") {
				poison = true
			}
		}
		if !poison {
			return sdk.PassRequest(), nil
		}
		out := proto.Clone(req).(*pb.ChatRequest)
		out.Messages = append(out.Messages, &pb.Message{
			Role: "assistant",
			Blocks: []*pb.RequestBlock{{
				Kind: &pb.RequestBlock_RedactedThinking{
					RedactedThinking: &pb.RequestRedactedThinkingBlock{Data: "REDACTED"},
				},
			}},
		})
		return sdk.ReplaceRequest(out), nil
	})
}
