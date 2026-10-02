package wasm

import (
	"context"
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
)

func TestToolResultReleaseUsesAcceptedInputAndBundle(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.host_call.torana_tool_result_release")
	p.SetBundleDigest("sha256:bundle")
	result := &pb.RequestToolResultBlock{ToolCallId: "original-call", Content: []*pb.ToolResultContentBlock{{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "synthetic content"}}}}}
	input := &pb.ChatRequest{Messages: []*pb.Message{{Role: "tool", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_ToolResult{ToolResult: result}}}}}}
	call := &pb.RequestToolUseBlock{Id: result.ToolCallId, Name: "Read", ArgumentsJson: []byte(`{"file_path":"src/config.json"}`)}
	input.Messages = append(input.Messages, &pb.Message{Role: "assistant", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_ToolUse{ToolUse: call}}}})
	ctx := context.WithValue(context.Background(), invocationHookKey{}, pb.Hook_HOOK_BEFORE_REQUEST)
	ctx = context.WithValue(ctx, releaseInputKey{}, input)
	calls := 0
	r.ToolResultReleaseFunc = func(_ context.Context, name, digest string, actual *pb.RequestToolResultBlock, actualCall *pb.RequestToolUseBlock, reason *sdk.ToolResultReleaseReason) ([]byte, *pb.HostError) {
		calls++
		if name != p.name || digest != "sha256:bundle" || actual != result || actualCall != call || reason == nil || reason.Kind != "scan_failure" {
			t.Fatalf("untrusted scope %s %s %+v", name, digest, actual)
		}
		return []byte(`{"reference":"","approved":false}`), nil
	}
	invoke := func(ctx context.Context, args string) *pb.HostCallResult {
		raw := r.dispatchHostCallForTest(ctx, p.name, "torana_tool_result_release", args)
		var frame pb.HostCallResult
		if err := proto.Unmarshal(raw, &frame); err != nil {
			t.Fatal(err)
		}
		return &frame
	}
	if got := invoke(ctx, `{"message":0,"block":0,"register":true,"reason":{"kind":"scan_failure"}}`); got.GetError() != nil {
		t.Fatal(got.GetError())
	}
	for _, args := range []string{`{"message":0,"block":0,"register":true}`, `{"message":0,"block":0,"register":false,"reason":{"kind":"scan_failure"}}`, `{"message":0,"block":0,"register":true,"reason":{"kind":"findings","findings":[{"type":"api_key","line":1,"value":"secret"}]}}`} {
		if got := invoke(ctx, args); got.GetError().GetCode() != pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
			t.Fatalf("accepted invalid review context: %s", args)
		}
	}
	for _, args := range []string{`{"message":-1,"block":0,"register":true}`, `{"message":0,"block":1,"register":true}`, `{"message":0,"block":0,"register":true,"conversation":"other"}`, `{"message":0,"message":1,"block":0,"register":true}`, `{"message":0,"block":0}`} {
		if got := invoke(ctx, args); got.GetError().GetCode() != pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
			t.Fatalf("accepted %s: %v", args, got)
		}
	}
	if calls != 1 {
		t.Fatalf("callback saw invalid input: %d", calls)
	}
	p.SetGrants(nil)
	if got := invoke(ctx, `{"message":0,"block":0,"register":true}`); got.GetError().GetCode() != pb.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Fatalf("ungranted=%v", got)
	}
	p.SetGrants([]string{"env.host_call.torana_tool_result_release"})
	if got := invoke(context.WithValue(ctx, invocationHookKey{}, pb.Hook_HOOK_ON_HTTP_REQUEST), `{"message":0,"block":0,"register":true}`); got.GetError() == nil {
		t.Fatal("HTTP callback registered result")
	}
}
