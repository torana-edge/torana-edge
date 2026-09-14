package pbconv

import (
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestToolErrorFlagSurvivesHostIR(t *testing.T) {
	for _, value := range []*bool{nil, proto.Bool(false), proto.Bool(true)} {
		in := &pb.ChatRequest{Messages: []*pb.Message{{Role: "user", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_ToolResult{ToolResult: &pb.RequestToolResultBlock{
			ToolCallId: "call_1", IsError: value, Content: []*pb.ToolResultContentBlock{{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "diagnostic"}}}},
		}}}}}}}
		host, err := FromPBChatRequest(in)
		if err != nil {
			t.Fatal(err)
		}
		out, err := ToPBChatRequestChecked(host)
		if err != nil {
			t.Fatal(err)
		}
		if got := out.Messages[0].Blocks[0].GetToolResult().IsError; (got == nil) != (value == nil) || (got != nil && *got != *value) {
			t.Errorf("is_error=%v became %v after PB -> host IR -> PB", value, got)
		}
		if value != nil {
			*out.Messages[0].Blocks[0].GetToolResult().IsError = !*value
			if *host.Messages[0].Blocks[0].ToolResult.IsError != *value {
				t.Fatal("PB output aliases host error flag")
			}
			*host.Messages[0].Blocks[0].ToolResult.IsError = !*value
			if *in.Messages[0].Blocks[0].GetToolResult().IsError != *value {
				t.Fatal("host IR aliases PB input error flag")
			}
		}
	}
}
