package plugin

import (
	"strings"
	"testing"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
)

func freeformMutationRequest() *pb.ChatRequest {
	input := "echo before"
	return &pb.ChatRequest{Model: "gpt", Messages: []*pb.Message{
		{Role: "assistant", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_ToolUse{ToolUse: &pb.RequestToolUseBlock{
			Id: "c1", Name: "exec", InputText: &input,
			InvocationKind: pb.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM, Signature: "call-token",
		}}}}},
		{Role: "tool", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_ToolResult{ToolResult: &pb.RequestToolResultBlock{
			ToolCallId: "c1", ToolName: "exec", InvocationKind: pb.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM,
			Content: []*pb.ToolResultContentBlock{{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "before"}}}},
		}}}}},
	}}
}

func onlyCustomToolGrants(grants ...string) func(string) bool {
	set := make(map[string]bool, len(grants))
	for _, grant := range grants {
		set[grant] = true
	}
	return func(section string) bool { return set[section] }
}

func TestFreeformCallMutationRequiresRoleGrantAndClearedProvenance(t *testing.T) {
	accepted := freeformMutationRequest()
	out := proto.Clone(accepted).(*pb.ChatRequest)
	changed := "echo after"
	out.Messages[0].Blocks[0].GetToolUse().InputText = &changed
	out.Messages[0].Blocks[0].GetToolUse().Signature = ""
	if err := verifyRequestMutation(accepted, out, onlyCustomToolGrants("ir.messages.write.assistant")); err != nil {
		t.Fatalf("authorized free-form input mutation rejected: %v", err)
	}
	if err := verifyRequestMutation(accepted, out, onlyCustomToolGrants()); err == nil || !strings.Contains(err.Error(), "ir.messages.write.assistant") {
		t.Fatalf("grant-less mutation error = %v", err)
	}
	stale := proto.Clone(out).(*pb.ChatRequest)
	stale.Messages[0].Blocks[0].GetToolUse().Signature = "call-token"
	if err := verifyRequestMutation(accepted, stale, onlyCustomToolGrants("ir.messages.write.assistant")); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale free-form signature accepted: %v", err)
	}
}

func TestFreeformResultTextUsesToolResultsGrant(t *testing.T) {
	accepted := freeformMutationRequest()
	out := proto.Clone(accepted).(*pb.ChatRequest)
	out.Messages[1].Blocks[0].GetToolResult().Content[0].GetText().Text = "after"
	if err := verifyRequestMutation(accepted, out, onlyCustomToolGrants("ir.tool_results.write")); err != nil {
		t.Fatalf("free-form result text rejected with exact grant: %v", err)
	}
	if err := verifyRequestMutation(accepted, out, onlyCustomToolGrants("ir.messages.write.tool")); err == nil || !strings.Contains(err.Error(), "ir.tool_results.write") {
		t.Fatalf("role grant authorized carved-out result text: %v", err)
	}
}

func TestToolNamespacePathIsHostOwned(t *testing.T) {
	accepted := &pb.ChatRequest{Tools: []*pb.ToolDef{{
		Name: "exec", InvocationKind: pb.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM,
		InputFormatJson: []byte(`{}`), NamespacePath: []string{"functions"},
	}}}
	out := proto.Clone(accepted).(*pb.ChatRequest)
	out.Tools[0].NamespacePath = []string{"other"}
	err := verifyRequestMutation(accepted, out, onlyCustomToolGrants("ir.tools.write"))
	if err == nil || !strings.Contains(err.Error(), "host-owned") {
		t.Fatalf("namespace topology changed under tools grant: %v", err)
	}
}

func TestToolFingerprintFramesToolAndNamespaceCardinality(t *testing.T) {
	// Without explicit cardinalities these two requests produced the same
	// framed value sequence: the six namespace segments on the first request
	// can spell the six fixed fields of a second tool in the other request.
	first := &pb.ToolDef{Name: "one", Description: "d", ParametersJson: []byte(`{}`),
		NamespacePath: []string{"two", "d2", "{}", "\x00", "0", ""}}
	oneTool := &pb.ChatRequest{Tools: []*pb.ToolDef{first}}
	twoTools := &pb.ChatRequest{Tools: []*pb.ToolDef{
		{Name: "one", Description: "d", ParametersJson: []byte(`{}`)},
		{Name: "two", Description: "d2", ParametersJson: []byte(`{}`)},
	}}
	a, err := fingerprintRequestSections(oneTool)
	if err != nil {
		t.Fatal(err)
	}
	b, err := fingerprintRequestSections(twoTools)
	if err != nil {
		t.Fatal(err)
	}
	if a.tools == b.tools {
		t.Fatal("tool/namespace cardinality boundary is absent from the tools fingerprint")
	}
}
