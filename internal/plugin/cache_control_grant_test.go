package plugin

import (
	"strings"
	"testing"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The ir.cache_control.write authority tests (prerequisite PR matrix):
//
//  1. message cache-control-only mutation succeeds with ONLY that grant, for
//     every modeled role and "other";
//  2. tool-def cache-control-only mutation succeeds with only that grant;
//  3. the same mutations fail without it, even with every message-role grant
//     and/or ir.tools.write;
//  4. holding only it cannot authorize content/role/content-parts/thinking/
//     redacted-thinking/tool-call fields or any signature mutation;
//  5. holding only it cannot authorize tool name/description/parameters/
//     strict, model, params, stream, topology, host metadata, unknown fields,
//     or signature/provenance violations;
//  6. mixed cache-control + content/schema changes require the UNION of
//     grants; one missing rejects the entire output atomically;
//  7. reorder/add/delete charge cache-control plus structural authorities and
//     a marker moved between positions with unchanged bytes cannot evade;
//  8. the all-grants fast path requires the new grant and still runs the
//     unconditional invariants;
//  9. manifest validation and lint accept the new permission.

const ccGrant = "ir.cache_control.write"

// marker returns the ordinary ephemeral breakpoint bytes used by fixtures.
func marker() []byte { return []byte(`{"type":"ephemeral"}`) }

// ccOnly grants exactly ir.cache_control.write.
func ccOnly(section string) bool { return section == ccGrant }

// toolResultsOnly grants exactly ir.tool_results.write.
func toolResultsOnly(section string) bool { return section == trGrant }

const trGrant = "ir.tool_results.write"

// allRoleAndToolGrants grants every message-role section and ir.tools.write,
// deliberately NOT ir.cache_control.write.
func allRoleAndToolGrants(section string) bool {
	switch section {
	case "ir.messages.write.user", "ir.messages.write.assistant",
		"ir.messages.write.system", "ir.messages.write.tool",
		"ir.messages.write.developer", "ir.messages.write.other",
		"ir.tools.write":
		return true
	}
	return false
}

// TestCacheControlOnlyMutationSucceedsPerRole — adding or changing a message
// breakpoint marker requires only ir.cache_control.write, for every modeled
// role and the unmodelled catch-all.
func TestCacheControlOnlyMutationSucceedsPerRole(t *testing.T) {
	for _, role := range []string{"user", "assistant", "system", "tool", "developer", "other"} {
		t.Run(role, func(t *testing.T) {
			accepted := &pb.ChatRequest{Messages: []*pb.Message{{Role: role, Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "c"}}}}}}}
			// Marker ADDED.
			out := &pb.ChatRequest{Messages: []*pb.Message{{Role: role, Blocks: []*pb.RequestBlock{
				{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "c"}}},
				{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: marker()}}},
			}}}}
			if err := verifyRequestMutation(accepted, out, ccOnly); err != nil {
				t.Fatalf("marker add with only ir.cache_control.write: %v", err)
			}
			// Marker CHANGED (same position).
			out2 := &pb.ChatRequest{Messages: []*pb.Message{{Role: role, Blocks: []*pb.RequestBlock{
				{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "c"}}},
				{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: []byte(`{"type":"ephemeral","ttl":"1h"}`)}}},
			}}}}
			if err := verifyRequestMutation(out, out2, ccOnly); err != nil {
				t.Fatalf("marker change with only ir.cache_control.write: %v", err)
			}
		})
	}
}

// TestToolDefCacheControlOnlyMutationSucceeds — tool-definition marker changes
// require only ir.cache_control.write.
func TestToolDefCacheControlOnlyMutationSucceeds(t *testing.T) {
	accepted := &pb.ChatRequest{Tools: []*pb.ToolDef{{Name: "read", Description: "d"}}}
	out := &pb.ChatRequest{Tools: []*pb.ToolDef{{Name: "read", Description: "d", CacheControlJson: marker()}}}
	if err := verifyRequestMutation(accepted, out, ccOnly); err != nil {
		t.Fatalf("tool marker add with only ir.cache_control.write: %v", err)
	}
	changed := &pb.ChatRequest{Tools: []*pb.ToolDef{{Name: "read", Description: "d", CacheControlJson: []byte(`{"type":"persistent"}`)}}}
	if err := verifyRequestMutation(out, changed, ccOnly); err != nil {
		t.Fatalf("tool marker change with only ir.cache_control.write: %v", err)
	}
}

// TestCacheControlMutationFailsWithoutTheGrant — the old authorities do not
// cover markers: with every role grant and ir.tools.write but WITHOUT
// ir.cache_control.write, marker mutations are rejected and name the missing
// grant.
func TestCacheControlMutationFailsWithoutTheGrant(t *testing.T) {
	accepted := &pb.ChatRequest{Messages: []*pb.Message{{Role: "user", Blocks: []*pb.RequestBlock{
		{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "c"}}},
	}}}}
	out := &pb.ChatRequest{Messages: []*pb.Message{{Role: "user", Blocks: []*pb.RequestBlock{
		{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "c"}}},
		{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: marker()}}},
	}}}}
	err := verifyRequestMutation(accepted, out, allRoleAndToolGrants)
	if err == nil {
		t.Fatal("marker mutation must fail without ir.cache_control.write even with every role grant")
	}
	if !strings.Contains(err.Error(), "ir.cache_control.write") {
		t.Fatalf("error does not name the missing grant: %v", err)
	}

	// Same on the tool side.
	tacc := &pb.ChatRequest{Tools: []*pb.ToolDef{{Name: "read", Description: "d"}}}
	tout := &pb.ChatRequest{Tools: []*pb.ToolDef{{Name: "read", Description: "d", CacheControlJson: marker()}}}
	err = verifyRequestMutation(tacc, tout, allRoleAndToolGrants)
	if err == nil || !strings.Contains(err.Error(), "ir.cache_control.write") {
		t.Fatalf("tool marker mutation must name the missing grant, got %v", err)
	}
}

// TestOnlyCacheControlGrantCannotAuthorizeOtherFields — with ONLY
// ir.cache_control.write, every non-marker mutation is rejected. The
// per-field sweep mirrors the reflection-driven mutation suite: each
// descriptor field of Message, ToolDef, RequestToolUseBlock, and the top-level request
// is mutated and must FAIL under the cc-only grant unless it is
// cache_control_json.
func TestOnlyCacheControlGrantCannotAuthorizeOtherFields(t *testing.T) {
	// Message fields.
	msgTables := []struct {
		name string
		pick func(*pb.ChatRequest) protoMessage
		skip map[string]bool
	}{
		{"Message", func(r *pb.ChatRequest) protoMessage { return r.Messages[1] }, map[string]bool{
			"cache_control_json": true, // the ONLY field the grant covers
		}},
		{"ToolDef", func(r *pb.ChatRequest) protoMessage { return r.Tools[0] }, map[string]bool{
			"cache_control_json": true,
		}},
		{"RequestToolUseBlock", func(r *pb.ChatRequest) protoMessage { return r.Messages[1].Blocks[1].GetToolUse() }, nil},
	}
	for _, tbl := range msgTables {
		t.Run(tbl.name, func(t *testing.T) {
			fields := tbl.pick(ccBaseRequest()).ProtoReflect().Descriptor().Fields()
			for i := 0; i < fields.Len(); i++ {
				fd := fields.Get(i)
				if tbl.skip[fd.TextName()] {
					continue
				}
				t.Run(fd.TextName(), func(t *testing.T) {
					accepted := ccBaseRequest()
					out := cloneChat(accepted)
					mutateField(t, tbl.pick(out), fd)
					if err := verifyRequestMutation(accepted, out, ccOnly); err == nil {
						t.Fatalf("field %s passed under only ir.cache_control.write", fd.TextName())
					}
				})
			}
		})
	}

	// Top-level request fields (model/params/stream/tools topology) plus the
	// host-owned metadata field.
	reqTables := []struct {
		name string
		mut  func(*pb.ChatRequest)
	}{
		{"model", func(r *pb.ChatRequest) { r.Model = "other" }},
		{"stream", func(r *pb.ChatRequest) { r.Stream = true }},
		{"max_tokens", func(r *pb.ChatRequest) { v := int32(5); r.MaxTokens = &v }},
		{"temperature", func(r *pb.ChatRequest) { v := 0.5; r.Temperature = &v }},
		{"top_p", func(r *pb.ChatRequest) { v := 0.5; r.TopP = &v }},
		{"stop_sequences", func(r *pb.ChatRequest) { r.StopSequences = []string{"x"} }},
		{"provider_extensions_json", func(r *pb.ChatRequest) { r.ProviderExtensionsJson = []byte(`{}`) }},
		{"safety_settings_json", func(r *pb.ChatRequest) { r.SafetySettingsJson = []byte(`{}`) }},
		{"tools add", func(r *pb.ChatRequest) { r.Tools = append(r.Tools, &pb.ToolDef{Name: "x"}) }},
		{"torana_meta_json", func(r *pb.ChatRequest) { r.ToranaMetaJson = []byte(`{"x":1}`) }},
	}
	for _, tbl := range reqTables {
		t.Run(tbl.name, func(t *testing.T) {
			accepted := ccBaseRequest()
			out := cloneChat(accepted)
			tbl.mut(out)
			if err := verifyRequestMutation(accepted, out, ccOnly); err == nil {
				t.Fatalf("%s passed under only ir.cache_control.write", tbl.name)
			}
		})
	}
}

// TestMixedCacheControlAndContentRequiresUnion — a marker + content change
// needs BOTH grants; with either alone the entire output is rejected
// atomically (the hook gets an error, so nothing is applied).
func TestMixedCacheControlAndContentRequiresUnion(t *testing.T) {
	accepted := &pb.ChatRequest{Messages: []*pb.Message{{Role: "user", Blocks: []*pb.RequestBlock{
		{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "c"}}},
	}}}}
	out := &pb.ChatRequest{Messages: []*pb.Message{{Role: "user", Blocks: []*pb.RequestBlock{
		{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "changed"}}},
		{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: marker()}}},
	}}}}

	if err := verifyRequestMutation(accepted, out, ccOnly); err == nil {
		t.Fatal("content change passed under only ir.cache_control.write")
	}
	contentOnly := func(section string) bool { return section == "ir.messages.write.user" }
	if err := verifyRequestMutation(accepted, out, contentOnly); err == nil {
		t.Fatal("marker change passed under only the content grant")
	}
	union := func(section string) bool { return ccOnly(section) || contentOnly(section) }
	if err := verifyRequestMutation(accepted, out, union); err != nil {
		t.Fatalf("union of grants must pass: %v", err)
	}

	// Tool side: marker + description change needs ir.cache_control.write +
	// ir.tools.write.
	tacc := &pb.ChatRequest{Tools: []*pb.ToolDef{{Name: "read", Description: "d"}}}
	tout := &pb.ChatRequest{Tools: []*pb.ToolDef{{Name: "read", Description: "changed", CacheControlJson: marker()}}}
	if err := verifyRequestMutation(tacc, tout, ccOnly); err == nil {
		t.Fatal("tool description change passed under only ir.cache_control.write")
	}
	toolsOnly := func(section string) bool { return section == "ir.tools.write" }
	if err := verifyRequestMutation(tacc, tout, toolsOnly); err == nil {
		t.Fatal("tool marker change passed under only ir.tools.write")
	}
	unionT := func(section string) bool { return ccOnly(section) || toolsOnly(section) }
	if err := verifyRequestMutation(tacc, tout, unionT); err != nil {
		t.Fatalf("union of tool grants must pass: %v", err)
	}
}

// TestMarkerMoveCannotEvade — moving an UNCHANGED marker between positions
// changes the cache-control section (the fingerprint folds the absolute
// index), so the move cannot slip through as "no marker change", and a
// marker-carrying message add/delete charges the section too.
func TestMarkerMoveCannotEvade(t *testing.T) {
	accepted := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "user", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "a"}}},
			{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: marker()}}},
		}},
		{Role: "user", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "b"}}},
		}},
	}}
	// Same bytes, marker moved to the other message.
	out := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "user", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "a"}}},
		}},
		{Role: "user", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "b"}}},
			{Kind: &pb.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pb.RequestCacheBreakpoint{MarkerJson: marker()}}},
		}},
	}}
	if err := verifyRequestMutation(accepted, out, ccOnly); err != nil {
		t.Fatalf("a marker move with unchanged bytes must be a cache-control change only, got: %v", err)
	}
	if err := verifyRequestMutation(accepted, out, allRoleAndToolGrants); err == nil {
		t.Fatal("a marker move must fail without ir.cache_control.write")
	}

	// Delete a marker-carrying message: cache-control AND the role section
	// change.
	del := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "user", Blocks: []*pb.RequestBlock{
			{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "b"}}},
		}},
	}}
	delRole := func(section string) bool { return section == "ir.messages.write.user" }
	if err := verifyRequestMutation(accepted, del, delRole); err == nil {
		t.Fatal("deleting a marker-carrying message must charge ir.cache_control.write")
	}
	delBoth := func(section string) bool { return delRole(section) || ccOnly(section) }
	if err := verifyRequestMutation(accepted, del, delBoth); err != nil {
		t.Fatalf("delete with both grants must pass: %v", err)
	}

	// ---- ToolDef markers: same positional rules through the FINGERPRINT
	// verifier ----
	toolAcc := &pb.ChatRequest{Tools: []*pb.ToolDef{
		{Name: "read", Description: "d"},
		{Name: "write", Description: "d", CacheControlJson: marker()},
	}}
	// Unchanged marker moved between tool indices.
	toolMoved := &pb.ChatRequest{Tools: []*pb.ToolDef{
		{Name: "read", Description: "d", CacheControlJson: marker()},
		{Name: "write", Description: "d"},
	}}
	if err := verifyRequestMutation(toolAcc, toolMoved, ccOnly); err != nil {
		t.Fatalf("a tool marker move with unchanged bytes must be a cache-control change only, got: %v", err)
	}
	if err := verifyRequestMutation(toolAcc, toolMoved, allRoleAndToolGrants); err == nil {
		t.Fatal("a tool marker move must fail without ir.cache_control.write")
	}
	// Marker-bearing tool ADD: cache-control AND ir.tools.write both charge.
	toolAdded := &pb.ChatRequest{Tools: []*pb.ToolDef{
		{Name: "read", Description: "d"},
		{Name: "write", Description: "d", CacheControlJson: marker()},
		{Name: "grep", Description: "d", CacheControlJson: marker()},
	}}
	toolsOnly := func(section string) bool { return section == "ir.tools.write" }
	if err := verifyRequestMutation(toolAcc, toolAdded, toolsOnly); err == nil {
		t.Fatal("adding a marker-bearing tool must charge ir.cache_control.write")
	}
	if err := verifyRequestMutation(toolAcc, toolAdded, ccOnly); err == nil {
		t.Fatal("adding a tool must charge ir.tools.write")
	}
	toolUnion := func(section string) bool { return toolsOnly(section) || ccOnly(section) }
	if err := verifyRequestMutation(toolAcc, toolAdded, toolUnion); err != nil {
		t.Fatalf("tool add with both grants must pass: %v", err)
	}
	// Marker-bearing tool DELETE: same union.
	toolDel := &pb.ChatRequest{Tools: []*pb.ToolDef{
		{Name: "read", Description: "d"},
	}}
	if err := verifyRequestMutation(toolAcc, toolDel, toolsOnly); err == nil {
		t.Fatal("deleting a marker-bearing tool must charge ir.cache_control.write")
	}
	if err := verifyRequestMutation(toolAcc, toolDel, toolUnion); err != nil {
		t.Fatalf("tool delete with both grants must pass: %v", err)
	}

	// ---- The comparison ORACLE must agree with the fingerprint on every
	// ToolDef case above (parallel positional implementations cannot drift).
	if !compareSections(toolAcc, toolMoved).cacheControl {
		t.Fatal("oracle: tool marker move not flagged as cacheControl")
	}
	if compareSections(toolAcc, toolMoved).tools {
		t.Fatal("oracle: tool marker move must not flag the tools section")
	}
	ccAdd := compareSections(toolAcc, toolAdded)
	if !ccAdd.cacheControl || !ccAdd.tools {
		t.Fatalf("oracle: tool add must flag cacheControl AND tools, got %+v", ccAdd)
	}
	ccDel := compareSections(toolAcc, toolDel)
	if !ccDel.cacheControl || !ccDel.tools {
		t.Fatalf("oracle: tool delete must flag cacheControl AND tools, got %+v", ccDel)
	}
	// And the message-side move for completeness.
	msgMoved := compareSections(accepted, out)
	if !msgMoved.cacheControl || len(msgMoved.messages) != 0 {
		t.Fatalf("oracle: message marker move must flag cacheControl only, got %+v", msgMoved)
	}
}

// TestHoldsAllRequestGrantsIncludesCacheControl — the all-grants fast path
// requires the new grant: a plugin with the old ten is not fast-path
// eligible, and the unconditional invariants still run for the full set.
func TestHoldsAllRequestGrantsIncludesCacheControl(t *testing.T) {
	has := func(perms ...string) *ccFakeGrants {
		set := map[string]bool{}
		for _, p := range perms {
			set[p] = true
		}
		return &ccFakeGrants{set: set}
	}
	if !holdsAllRequestGrants(has(allRequestGrants...)) {
		t.Fatal("the full grant set must hold the fast path")
	}
	var without []string
	for _, g := range allRequestGrants {
		if g != ccGrant {
			without = append(without, g)
		}
	}
	if holdsAllRequestGrants(has(without...)) {
		t.Fatal("the old ten-grant set must NOT hold the fast path")
	}
}

type ccFakeGrants struct{ set map[string]bool }

func (f *ccFakeGrants) HasGrant(p string) bool { return f.set[p] }

// TestManifestAcceptsTheGrant — manifest validation accepts
// ir.cache_control.write (supportedPermissions derives from sdk.Permissions,
// which the SDK PR extended). The LINTER's acceptance and attribution
// regressions live in internal/plugincmd (lint_test.go), which runs the real
// scanner; this package only validates the manifest.
func TestManifestAcceptsTheGrant(t *testing.T) {
	if _, ok := supportedPermissions[ccGrant]; !ok {
		t.Fatal("supportedPermissions does not include ir.cache_control.write")
	}
	m := PluginManifest{
		SchemaVersion: 1,
		ID:            "torana-test/cc-fixture",
		Name:          "cc-fixture",
		Version:       "0.1.0",
		ABIVersion:    "v1",
		FailureMode:   "pass",
		Hooks:         []Hook{{Name: "run_before_request"}},
		Permissions:   []Permission{{Name: ccGrant}},
	}
	if err := validateManifest(m); err != nil {
		t.Fatalf("manifest with ir.cache_control.write rejected: %v", err)
	}
}

// ccBaseRequest is a marker-less request with one message + one tool, for the
// negative sweeps.
func ccBaseRequest() *pb.ChatRequest {
	return &pb.ChatRequest{
		Model: "m",
		Messages: []*pb.Message{
			{Role: "system", Blocks: []*pb.RequestBlock{{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "s"}}}}},
			{Role: "user", Blocks: []*pb.RequestBlock{
				{Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: "u"}}},
				{Kind: &pb.RequestBlock_ToolUse{ToolUse: &pb.RequestToolUseBlock{Id: "c1", Name: "read", ArgumentsJson: []byte(`{}`)}}},
			}},
		},
		Tools: []*pb.ToolDef{{Name: "read", Description: "d", ParametersJson: []byte(`{}`)}},
	}
}

func cloneChat(r *pb.ChatRequest) *pb.ChatRequest {
	return proto.Clone(r).(*pb.ChatRequest)
}

// protoMessage is the mutation-suite message interface.
type protoMessage interface {
	ProtoReflect() protoreflect.Message
}
