package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
)

func main() {}

// Test fixture: appends a CONTRACT-VALID tool-role message (SDK:
// tool_result.tool_call_id required non-empty). The anthropic adapter
// refuses tool-role messages at marshal — so the accepted replacement
// triggers the HOST MARSHAL FAILURE terminal (host_error 500).
func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (sdk.RequestResult, error) {
		out := proto.Clone(req).(*pb.ChatRequest)
		out.Messages = append(out.Messages, &pb.Message{
			Role: "tool",
			Blocks: []*pb.RequestBlock{{
				Kind: &pb.RequestBlock_ToolResult{
					ToolResult: &pb.RequestToolResultBlock{
						ToolCallId: "c1",
						Content: []*pb.ToolResultContentBlock{{
							Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "ok"}},
						}},
					},
				},
			}},
		})
		return sdk.ReplaceRequest(out), nil
	})
}
