package plugin

import (
	"strings"
	"testing"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
)

// The replacement validation closure: a structurally invalid replacement is
// refused at DECODE — unconditionally, before any write-grant check, so the
// all-grants fast path can never see (or apply) a request that violates the
// SDK's absolute grammar.
func TestDecodeHookResultValidatesReplacementUnconditionally(t *testing.T) {
	cases := map[string]func() []byte{
		"nil block element": func() []byte {
			return mustEncodeRequest(t, &pb.ChatRequest{
				Messages: []*pb.Message{{
					Role:   "user",
					Blocks: []*pb.RequestBlock{nil},
				}},
			})
		},
		"empty blocks refused": func() []byte {
			return mustEncodeRequest(t, &pb.ChatRequest{
				Messages: []*pb.Message{{Role: "user"}},
			})
		},
		"missing role": func() []byte {
			return mustEncodeRequest(t, &pb.ChatRequest{
				Messages: []*pb.Message{{
					Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_Text{
						Text: &pb.RequestTextBlock{Text: "hi"},
					}}},
				}},
			})
		},
		"typed-nil tool use": func() []byte {
			return mustEncodeRequest(t, &pb.ChatRequest{
				Messages: []*pb.Message{{
					Role:   "assistant",
					Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_ToolUse{}}},
				}},
			})
		},
		"malformed arguments": func() []byte {
			return mustEncodeRequest(t, &pb.ChatRequest{
				Messages: []*pb.Message{{
					Role: "assistant",
					Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_ToolUse{
						ToolUse: &pb.RequestToolUseBlock{
							Id: "c1", Name: "f", ArgumentsJson: []byte(`[1,2]`),
						},
					}}},
				}},
			})
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := decodeHookResult(mk(), pb.Hook_HOOK_BEFORE_REQUEST)
			if err == nil {
				t.Fatalf("structurally invalid replacement decoded: %+v", res)
			}
			if !strings.Contains(err.Error(), "hook result:") {
				t.Errorf("error = %q, want the SDK contract named", err)
			}
		})
	}
}

// mustEncodeRequest wraps a request in a replace_request action frame.
func mustEncodeRequest(t *testing.T, req *pb.ChatRequest) []byte {
	t.Helper()
	res := &pb.HookResult{Action: &pb.HookResult_ReplaceRequest{ReplaceRequest: req}}
	b, err := proto.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
