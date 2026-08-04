package plugin

// Edge host-boundary proof for the SDK tool-result seam (SDK main
// 995c0bd, merged): the ir.tool_results.write grant model. A position-keyed
// tool-result text VALUE change is governed by the NEW grant alone — the
// role view placeholders the text values and normalizes the provider
// signature tokens and the trailing carrier (unconditional provenance), so
// the coupled helper mutation (text change + containing-result-signature
// clear, trailing carrier PRESERVED) must be accepted with ONLY
// ir.tool_results.write, and must be rejected by every other grant alone.

import (
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
)

// richAssistantMsg is the assistant-role trailing case: leading text, the
// designated tool-result block (identity + metadata + two markers + a text
// arm + a signature), an unrelated sibling result, trailing text, and the
// final trailing-signature block.
func richAssistantMsg() *pb.Message {
	return &pb.Message{Role: "assistant", Blocks: []*pb.RequestBlock{
		{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "leading"}}},
		{Kind: &pb.RequestBlock_ToolResult{ToolResult: &pb.RequestToolResultBlock{
			ToolCallId:       "c1",
			ToolName:         "read",
			WillContinue:     proto.Bool(false),
			PartMetadataJson: []byte(`{"outer":{"b":1,"a":2}}`),
			Signature:        "result-sig",
			Content: []*pb.ToolResultContentBlock{
				{Kind: &pb.ToolResultContentBlock_CacheBreakpoint{CacheBreakpoint: &pb.ToolResultCacheBreakpoint{MarkerJson: []byte(`{"type":"ephemeral"}`)}}},
				{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "before"}}},
				{Kind: &pb.ToolResultContentBlock_CacheBreakpoint{CacheBreakpoint: &pb.ToolResultCacheBreakpoint{MarkerJson: []byte(`{"type":"standard"}`)}}},
			},
		}}},
		{Kind: &pb.RequestBlock_ToolResult{ToolResult: &pb.RequestToolResultBlock{
			ToolCallId: "c2",
			ToolName:   "write",
			Content:    []*pb.ToolResultContentBlock{{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "sibling"}}}},
		}}},
		{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "trailing"}}},
		{Kind: &pb.RequestBlock_TrailingSignature{TrailingSignature: &pb.RequestTrailingSignatureBlock{Signature: "trailing-sig"}}},
	}}
}

func trReq(msg *pb.Message) *pb.ChatRequest {
	return &pb.ChatRequest{Model: "m", Messages: []*pb.Message{msg}}
}

// TestToolResultSeamVerifierGrantBoundary — the real write-grant verifier:
//
//   - the helper mutation (text value change + containing-result-signature
//     clear, trailing carrier preserved) is ACCEPTED with ONLY
//     ir.tool_results.write — no role grant, no cache grant, no provenance
//     movement;
//   - the byte-identical helper call is a structural no-op — every token
//     preserved — and passes;
//   - REMOVING the grant is REJECTED, proving the row exercises
//     authorization;
//   - the OLD model's acceptance is now the rejection row: the assistant
//     ROLE grant alone must NOT authorize a tool-result text change (the
//     role view placeholders the value);
//   - a forged stale token (content changed, signature kept) is REJECTED by
//     the unconditional invariants (SignatureStale);
//   - the negatives: prompt text, tool identity, will_continue, a marker,
//     and trailing-carrier removal over UNCHANGED preceding content are all
//     rejected under the new grant alone.
func TestToolResultSeamVerifierGrantBoundary(t *testing.T) {
	accepted := richAssistantMsg()
	out := proto.Clone(accepted).(*pb.Message)
	changed, err := sdk.ReplaceToolResultText(out, 1, "after")
	if err != nil || !changed {
		t.Fatalf("helper: changed=%v err=%v", changed, err)
	}
	// The helper cleared ONLY the containing result signature; the trailing
	// carrier, the sibling, the markers and the leading/trailing text are
	// preserved byte-for-byte.
	if got := out.Blocks[1].GetToolResult().Signature; got != "" {
		t.Fatalf("stale result signature survived: %q", got)
	}
	if ts := out.Blocks[4].GetTrailingSignature(); ts == nil || ts.Signature != "trailing-sig" {
		t.Fatalf("the trailing carrier must be preserved byte-for-byte, got %+v", ts)
	}
	if got := accepted.Blocks[2].GetToolResult().Content[0].GetText().Text; got != "sibling" {
		t.Fatalf("sibling result disturbed: %q", got)
	}
	if got := accepted.Blocks[1].GetToolResult().Content[0].GetCacheBreakpoint().MarkerJson; string(got) != `{"type":"ephemeral"}` {
		t.Fatalf("marker disturbed: %s", got)
	}
	req := trReq(accepted)
	outReq := trReq(out)

	if err := verifyRequestMutation(req, outReq, toolResultsOnly); err != nil {
		t.Fatalf("the coupled mutation must be accepted with only ir.tool_results.write: %v", err)
	}

	// No-op: byte-identical helper call preserves everything; the verifier
	// sees no change at all.
	noop := proto.Clone(accepted).(*pb.Message)
	changed, err = sdk.ReplaceToolResultText(noop, 1, "before")
	if err != nil || changed {
		t.Fatalf("no-op: changed=%v err=%v", changed, err)
	}
	if !proto.Equal(noop, accepted) {
		t.Fatal("the no-op disturbed the message")
	}
	if err := verifyRequestMutation(req, trReq(noop), toolResultsOnly); err != nil {
		t.Fatalf("the no-op must pass the verifier: %v", err)
	}

	// Grant removal: REJECTED.
	noGrants := func(section string) bool { return false }
	if err := verifyRequestMutation(req, outReq, noGrants); err == nil {
		t.Fatal("removing the grant must reject the mutation")
	}

	// The OLD model is now the rejection row: a tool-result text change with
	// only the ROLE grant must be refused — the value is placeholdered out of
	// the role view, so the role grant never authorizes it.
	assistantOnly := func(section string) bool { return section == "ir.messages.write.assistant" }
	if err := verifyRequestMutation(req, outReq, assistantOnly); err == nil {
		t.Fatal("a tool-result text change must NOT be accepted with only the assistant grant")
	}

	// Forged stale token: content changed, signature KEPT — the
	// unconditional invariants reject it (SignatureStale).
	forged := proto.Clone(accepted).(*pb.Message)
	_, _ = sdk.ReplaceToolResultText(forged, 1, "after")
	forged.Blocks[1].GetToolResult().Signature = "result-sig" // forge it back
	if err := verifyRequestMutation(req, trReq(forged), toolResultsOnly); err == nil {
		t.Fatal("a forged stale signature must be rejected")
	}

	// Negatives under the new grant alone — every non-text-value change
	// stays governed by its own section or by provenance:
	t.Run("negatives", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(*pb.Message)
		}{
			{"prompt text", func(m *pb.Message) { m.Blocks[0].GetText().Text = "changed" }},
			{"tool identity", func(m *pb.Message) { m.Blocks[1].GetToolResult().ToolName = "write" }},
			{"will_continue", func(m *pb.Message) { w := true; m.Blocks[1].GetToolResult().WillContinue = &w }},
			{"nested marker", func(m *pb.Message) {
				m.Blocks[1].GetToolResult().Content[0].GetCacheBreakpoint().MarkerJson = []byte(`{"type":"1h"}`)
			}},
			{"trailing carrier removed", func(m *pb.Message) { m.Blocks = m.Blocks[:4] }},
			{"trailing present-empty token", func(m *pb.Message) { m.Blocks[4].GetTrailingSignature().Signature = "" }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				neg := proto.Clone(accepted).(*pb.Message)
				tc.mutate(neg)
				if err := verifyRequestMutation(req, trReq(neg), toolResultsOnly); err == nil {
					t.Fatalf("%s must be rejected under only ir.tool_results.write", tc.name)
				}
			})
		}
	})
}
