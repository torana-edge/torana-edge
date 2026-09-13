package pbconv

import (
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestToolErrorFlagSurvivesHostIR(t *testing.T) {
	for _, value := range []bool{false, true} {
		in := &pb.ChatRequest{Messages: []*pb.Message{{Role: "user", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_ToolResult{ToolResult: &pb.RequestToolResultBlock{
			ToolCallId: "call_1", IsError: proto.Bool(value), Content: []*pb.ToolResultContentBlock{{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "diagnostic"}}}},
		}}}}}}}
		host, err := FromPBChatRequest(in)
		if err != nil {
			t.Fatal(err)
		}
		out, err := ToPBChatRequestChecked(host)
		if err != nil {
			t.Fatal(err)
		}
		if got := out.Messages[0].Blocks[0].GetToolResult().IsError; got == nil || *got != value {
			t.Errorf("is_error=%v became %v after PB -> host IR -> PB", value, got)
		}
	}
}
