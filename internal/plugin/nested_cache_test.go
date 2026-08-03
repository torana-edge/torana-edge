package plugin

import (
	"strings"
	"testing"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// Nested tool-result cache markers (ToolResultContentBlock.cache_breakpoint)
// must be governed by ir.cache_control.write EXACTLY like top-level markers:
// the role sections must not see them (bodyWithoutCacheBlocks strips them
// recursively), the cache section must see their value AND position at the
// nested level, and the three carriers (top-level message, nested
// tool-result, tool-definition) must never collide in the fingerprint.
//
// These rows were written BEFORE the production fix: the role-section
// fingerprint still contained nested markers and the cache section never
// visited them.

// nestedBase returns a request whose assistant turn carries a tool result
// with one nested text element, and the tool definition the call used.
func nestedBase() *pb.ChatRequest {
	r := ccBaseRequest()
	// ccBaseRequest: [0]=system text, [1]=user text+toolUse, tools[0]=read.
	r.Messages[1].Blocks = append(r.Messages[1].Blocks, &pb.RequestBlock{
		Kind: &pb.RequestBlock_ToolResult{ToolResult: &pb.RequestToolResultBlock{
			ToolCallId: "c1",
			Content: []*pb.ToolResultContentBlock{
				{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "a"}}},
			},
		}},
	})
	return r
}

// nestedWith appends a nested cache breakpoint to the tool result of the
// user message (message index 1).
func nestedWith(r *pb.ChatRequest, markerJSON []byte) *pb.ChatRequest {
	out := cloneChat(r)
	tr := out.Messages[1].Blocks[2].GetToolResult()
	tr.Content = append(tr.Content, &pb.ToolResultContentBlock{
		Kind: &pb.ToolResultContentBlock_CacheBreakpoint{
			CacheBreakpoint: &pb.ToolResultCacheBreakpoint{MarkerJson: markerJSON},
		},
	})
	return out
}

// TestNestedCacheMarkerAddChangeDelete — the nested marker is cache-granted:
// add/change/delete succeed with ONLY ir.cache_control.write and fail with
// every role grant but no cache grant, naming the missing grant.
func TestNestedCacheMarkerAddChangeDelete(t *testing.T) {
	t.Run("add", func(t *testing.T) {
		accepted := nestedBase()
		out := nestedWith(accepted, marker())
		if err := verifyRequestMutation(accepted, out, ccOnly); err != nil {
			t.Fatalf("nested marker add with only ir.cache_control.write: %v", err)
		}
		err := verifyRequestMutation(accepted, out, allRoleAndToolGrants)
		if err == nil {
			t.Fatal("nested marker add must fail without ir.cache_control.write")
		}
		if !strings.Contains(err.Error(), "ir.cache_control.write") {
			t.Fatalf("error does not name the missing grant: %v", err)
		}
	})
	t.Run("change", func(t *testing.T) {
		accepted := nestedWith(nestedBase(), marker())
		out := nestedWith(nestedBase(), []byte(`{"type":"ephemeral","ttl":"1h"}`))
		if err := verifyRequestMutation(accepted, out, ccOnly); err != nil {
			t.Fatalf("nested marker change with only ir.cache_control.write: %v", err)
		}
		if err := verifyRequestMutation(accepted, out, allRoleAndToolGrants); err == nil {
			t.Fatal("nested marker change must fail without ir.cache_control.write")
		}
	})
	t.Run("delete", func(t *testing.T) {
		accepted := nestedWith(nestedBase(), marker())
		out := nestedBase()
		if err := verifyRequestMutation(accepted, out, ccOnly); err != nil {
			t.Fatalf("nested marker delete with only ir.cache_control.write: %v", err)
		}
		if err := verifyRequestMutation(accepted, out, allRoleAndToolGrants); err == nil {
			t.Fatal("nested marker delete must fail without ir.cache_control.write")
		}
	})
}

// TestNestedAdjacentTextChangeIsRoleOnly — changing the nested text NEXT TO
// an unchanged marker is role business: the marker authority covers marker
// value/position, never surrounding content.
func TestNestedAdjacentTextChangeIsRoleOnly(t *testing.T) {
	accepted := nestedWith(nestedBase(), marker())
	out := nestedBase()
	out.Messages[1].Blocks[2].GetToolResult().Content[0].GetText().Text = "a'"
	out = nestedWith(out, marker())

	if err := verifyRequestMutation(accepted, out, allRoleAndToolGrants); err != nil {
		t.Fatalf("adjacent nested text change with role grants: %v", err)
	}
	if err := verifyRequestMutation(accepted, out, ccOnly); err == nil {
		t.Fatal("adjacent nested text change must fail under only ir.cache_control.write")
	}
}

// TestNestedMarkerMoveCannotEvade — an unchanged nested marker moved between
// nested positions changes the cache section (position is part of the
// authority) and must not change the role sections.
func TestNestedMarkerMoveCannotEvade(t *testing.T) {
	accepted := nestedBase()
	accepted.Messages[1].Blocks[2].GetToolResult().Content = append(
		accepted.Messages[1].Blocks[2].GetToolResult().Content,
		&pb.ToolResultContentBlock{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "b"}}},
	)
	accepted = nestedWith(accepted, marker())

	// Move the marker from nested position 1 to nested position 2.
	out := cloneChat(accepted)
	tr := out.Messages[1].Blocks[2].GetToolResult()
	cb := tr.Content[1]
	tr.Content = append(tr.Content[:1], tr.Content[2:]...)
	tr.Content = append(tr.Content, cb)

	if err := verifyRequestMutation(accepted, out, ccOnly); err != nil {
		t.Fatalf("nested marker move with only ir.cache_control.write: %v", err)
	}
	if err := verifyRequestMutation(accepted, out, allRoleAndToolGrants); err == nil {
		t.Fatal("an unchanged nested marker moved between positions must fail without ir.cache_control.write")
	}
	// The role view must be untouched: the same move with the union passes.
	union := func(section string) bool { return ccOnly(section) || allRoleAndToolGrants(section) }
	if err := verifyRequestMutation(accepted, out, union); err != nil {
		t.Fatalf("nested marker move must not change the role sections: %v", err)
	}
}

// TestNestedMarkerMovePlusContentRequiresUnion — moving a nested marker AND
// changing surrounding content needs the UNION of the role and cache grants.
func TestNestedMarkerMovePlusContentRequiresUnion(t *testing.T) {
	accepted := nestedBase()
	accepted.Messages[1].Blocks[2].GetToolResult().Content = append(
		accepted.Messages[1].Blocks[2].GetToolResult().Content,
		&pb.ToolResultContentBlock{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "b"}}},
	)
	accepted = nestedWith(accepted, marker())

	out := cloneChat(accepted)
	tr := out.Messages[1].Blocks[2].GetToolResult()
	tr.Content[1].GetText().Text = "b'" // adjacent content change
	cb := tr.Content[2]
	tr.Content = []*pb.ToolResultContentBlock{tr.Content[0], cb, tr.Content[1]} // marker moved to position 1

	if err := verifyRequestMutation(accepted, out, ccOnly); err == nil {
		t.Fatal("content change must not pass under only ir.cache_control.write")
	}
	if err := verifyRequestMutation(accepted, out, allRoleAndToolGrants); err == nil {
		t.Fatal("marker move must not pass without ir.cache_control.write")
	}
	union := func(section string) bool { return ccOnly(section) || allRoleAndToolGrants(section) }
	if err := verifyRequestMutation(accepted, out, union); err != nil {
		t.Fatalf("the union of grants must pass: %v", err)
	}
}

// TestCacheCarriersNeverCollide — the same marker BYTES at the top-level
// message carrier, the nested tool-result carrier, and the tool-definition
// carrier must produce three distinct cache sections, and a mutation that
// moves bytes between carriers must be a cache-section change.
func TestCacheCarriersNeverCollide(t *testing.T) {
	same := []byte(`{"type":"ephemeral"}`)

	topLevel := ccBaseRequest()
	topLevel.Messages[1].Blocks = append(topLevel.Messages[1].Blocks,
		&pb.RequestBlock{Kind: &pb.RequestBlock_CacheBreakpoint{
			CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: same},
		}})

	nested := nestedWith(nestedBase(), same)

	toolDef := ccBaseRequest()
	toolDef.Tools[0].CacheControlJson = same

	topFp := fingerprintCacheControlSection(topLevel)
	nestedFp := fingerprintCacheControlSection(nested)
	toolFp := fingerprintCacheControlSection(toolDef)

	if topFp == nestedFp {
		t.Fatal("top-level and nested carriers with identical marker bytes collided")
	}
	if topFp == toolFp {
		t.Fatal("top-level and tool-definition carriers with identical marker bytes collided")
	}
	if nestedFp == toolFp {
		t.Fatal("nested and tool-definition carriers with identical marker bytes collided")
	}

	// Behavioral: a pure cross-carrier move (top-level -> nested) with
	// IDENTICAL non-marker content is a cache change only — the role views
	// strip both carriers, so the role sections never move.
	acc := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{
		{Role: "user", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "u"}}},
			{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: same}}},
		}},
		{Role: "user", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "v"}}},
			{Kind: &pb.RequestBlock_ToolResult{ToolResult: &pb.RequestToolResultBlock{
				ToolCallId: "c1",
				Content:    []*pb.ToolResultContentBlock{{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "a"}}}},
			}}},
		}},
	}}
	out := &pb.ChatRequest{Model: "m", Messages: []*pb.Message{
		{Role: "user", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "u"}}},
		}},
		{Role: "user", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "v"}}},
			{Kind: &pb.RequestBlock_ToolResult{ToolResult: &pb.RequestToolResultBlock{
				ToolCallId: "c1",
				Content: []*pb.ToolResultContentBlock{
					{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "a"}}},
					{Kind: &pb.ToolResultContentBlock_CacheBreakpoint{CacheBreakpoint: &pb.ToolResultCacheBreakpoint{MarkerJson: same}}},
				},
			}}},
		}},
	}}
	if err := verifyRequestMutation(acc, out, ccOnly); err != nil {
		t.Fatalf("cross-carrier move with only ir.cache_control.write: %v", err)
	}
	if err := verifyRequestMutation(acc, out, allRoleAndToolGrants); err == nil {
		t.Fatal("cross-carrier move must fail without ir.cache_control.write")
	}
}

// TestNestedCacheOracleAgreement — the comparison/reference oracle and the
// production fingerprints must agree for every nested-marker row: the cache
// section changes exactly when cacheControlChanged reports it, and the role
// sections change exactly when the message comparison reports them.
func TestNestedCacheOracleAgreement(t *testing.T) {
	type row struct {
		name          string
		accepted, out *pb.ChatRequest
	}
	baseA := nestedBase()
	baseOut := nestedWith(baseA, marker())
	rows := []row{
		{"nested add", baseA, nestedWith(nestedBase(), marker())},
		{"nested change", nestedWith(nestedBase(), marker()), nestedWith(nestedBase(), []byte(`{"type":"ephemeral","ttl":"1h"}`))},
		{"nested delete", nestedWith(nestedBase(), marker()), nestedBase()},
		{"adjacent text change", baseOut, func() *pb.ChatRequest {
			o := nestedWith(nestedBase(), marker())
			o.Messages[1].Blocks[2].GetToolResult().Content[0].GetText().Text = "a'"
			return o
		}()},
		{"nested move", func() *pb.ChatRequest {
			a := nestedBase()
			a.Messages[1].Blocks[2].GetToolResult().Content = append(
				a.Messages[1].Blocks[2].GetToolResult().Content,
				&pb.ToolResultContentBlock{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "b"}}},
			)
			return nestedWith(a, marker())
		}(), nil},
	}
	// Build the "out" side of the nested-move row (marker at position 2).
	moveAcc := rows[4].accepted
	moveOut := cloneChat(moveAcc)
	tr := moveOut.Messages[1].Blocks[2].GetToolResult()
	cb := tr.Content[1]
	tr.Content = append(tr.Content[:1], tr.Content[2:]...)
	tr.Content = append(tr.Content, cb)
	rows[4].out = moveOut

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c := compareSections(r.accepted, r.out)
			roleChanged := len(c.messages) > 0
			cacheChanged := c.cacheControl

			fpA := fingerprintRequestSections(r.accepted)
			fpO := fingerprintRequestSections(r.out)
			roleFpChanged := !sameRoleSections(fpA, fpO)

			ccA := fingerprintCacheControlSection(r.accepted)
			ccO := fingerprintCacheControlSection(r.out)
			cacheFpChanged := ccA != ccO

			if roleChanged != roleFpChanged {
				t.Fatalf("oracle role change %v != production role change %v", roleChanged, roleFpChanged)
			}
			if cacheChanged != cacheFpChanged {
				t.Fatalf("oracle cache change %v != production cache change %v", cacheChanged, cacheFpChanged)
			}
		})
	}
}

// sameRoleSections compares only the per-role message digests of two
// section fingerprints (the cache/tools/model/params sections are checked
// separately by the oracle agreement rows).
func sameRoleSections(a, b requestSections) bool {
	if len(a.messages) != len(b.messages) {
		return false
	}
	for role, digest := range a.messages {
		if b.messages[role] != digest {
			return false
		}
	}
	return true
}
