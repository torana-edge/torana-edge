package plugin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
)

// grant builds the canWrite predicate for verifyRequestMutation from the
// granted permission names.
func grant(permissions ...string) func(string) bool {
	held := make(map[string]bool, len(permissions))
	for _, p := range permissions {
		held[p] = true
	}
	return func(section string) bool { return held[section] }
}

// fakeGrants is a grants implementation backed by a plain map, for the fast
// path predicate.
type fakeGrants map[string]bool

func (f fakeGrants) HasGrant(perm string) bool { return f[perm] }

func TestVerifyRequestMutationGrantedChangeAccepted(t *testing.T) {
	cases := []struct {
		name   string
		base   func() *pb.ChatRequest
		mutate func(*pb.ChatRequest)
		perm   string
	}{
		{name: "message", perm: "ir.messages.write.user",
			mutate: func(r *pb.ChatRequest) { r.Messages[1].Content = "A'" }},
		{name: "tool", perm: "ir.tools.write",
			mutate: func(r *pb.ChatRequest) { r.Tools[0].ParametersJson = []byte(`{"type":"array"}`) }},
		{name: "model", perm: "ir.model.write",
			mutate: func(r *pb.ChatRequest) { r.Model = "claude-opus-4" }},
		{name: "params", perm: "ir.params.write",
			mutate: func(r *pb.ChatRequest) {
				v := 1.0
				r.Temperature = &v
			}},
		// The role is part of the message section: changing it marks the role
		// that left the slot AND the role that took it, so the developer case
		// starts from a request whose message is already developer.
		{name: "developer-role", perm: "ir.messages.write.developer",
			base: func() *pb.ChatRequest {
				r := baseRequest()
				r.Messages[1] = &pb.Message{Role: "developer", Content: "dev"}
				return r
			},
			mutate: func(r *pb.ChatRequest) { r.Messages[1].Content = "dev'" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := baseRequest
			if tc.base != nil {
				base = tc.base
			}
			accepted := base()
			out := base()
			tc.mutate(out)

			if err := verifyRequestMutation(accepted, out, grant(tc.perm)); err != nil {
				t.Fatalf("a change covered by %s must be accepted, got: %v", tc.perm, err)
			}
		})
	}
}

// An untouched request needs no grant at all.
func TestVerifyRequestMutationUnchangedNeedsNoGrant(t *testing.T) {
	req := baseRequest()
	if err := verifyRequestMutation(req, req, grant()); err != nil {
		t.Fatalf("an unmodified replacement must verify with no grants, got: %v", err)
	}
}

func TestVerifyRequestMutationUngrantedRejected(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*pb.ChatRequest)
		want   string
	}{
		{name: "message", want: "plugin changed messages.user without ir.messages.write.user",
			mutate: func(r *pb.ChatRequest) { r.Messages[1].Content = "A'" }},
		{name: "tool", want: "plugin changed tools without ir.tools.write",
			mutate: func(r *pb.ChatRequest) { r.Tools[0].ParametersJson = []byte(`{"type":"array"}`) }},
		{name: "model", want: "plugin changed model without ir.model.write",
			mutate: func(r *pb.ChatRequest) { r.Model = "claude-opus-4" }},
		{name: "params", want: "plugin changed params without ir.params.write",
			mutate: func(r *pb.ChatRequest) {
				v := 1.0
				r.Temperature = &v
			}},
		{name: "unmodelled-role", want: "plugin changed messages.weird without ir.messages.write.other",
			mutate: func(r *pb.ChatRequest) {
				r.Messages[1].Role = "weird"
				r.Messages[1].Content = "A'"
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accepted := baseRequest()
			out := baseRequest()
			tc.mutate(out)

			err := verifyRequestMutation(accepted, out, grant())
			if err == nil {
				t.Fatal("a change with no grant must be rejected")
			}
			if err.Error() != tc.want {
				t.Errorf("error = %q, want %q", err, tc.want)
			}
		})
	}
}

// A change in two sections is rejected at the first ungranted one, in the
// documented order: host-owned, messages, tools, model, params, signatures.
func TestVerifyRequestMutationFirstUngrantedSectionNamed(t *testing.T) {
	accepted := baseRequest()
	out := baseRequest()
	out.Messages[1].Content = "A'" // ungranted
	out.Tools[0].Name = "write"    // would be granted
	out.Model = "claude-opus-4"    // would be granted
	temp := 1.0
	out.Temperature = &temp // would be granted

	err := verifyRequestMutation(accepted, out, grant("ir.tools.write", "ir.model.write", "ir.params.write"))
	if err == nil {
		t.Fatal("a mixed replacement with one ungranted section must be rejected")
	}
	if err.Error() != "plugin changed messages.user without ir.messages.write.user" {
		t.Errorf("error = %q, want the first ungranted section named", err)
	}
}

// torana_meta_json is host-owned: changing it is a violation no grant covers,
// so even a plugin holding every request grant must be rejected.
func TestVerifyRequestMutationHostOwnedMetaAlwaysViolation(t *testing.T) {
	accepted := baseRequest()
	out := baseRequest()
	out.ToranaMetaJson = []byte(`{"_provider":"evil"}`)

	all := grant("ir.messages.write.user", "ir.messages.write.assistant",
		"ir.messages.write.system", "ir.messages.write.tool",
		"ir.messages.write.developer", "ir.messages.write.other",
		"ir.tools.write", "ir.model.write", "ir.params.write")
	err := verifyRequestMutation(accepted, out, all)
	if err == nil {
		t.Fatal("a torana_meta_json change must be a violation even with all grants")
	}
	if err.Error() != "plugin changed host-owned torana_meta_json" {
		t.Errorf("error = %q, want the host-owned violation named", err)
	}
}

// contentSignedRequest carries only a content_signature; thinkingSignedRequest
// only a thinking_signature (over thinking + redacted_thinking);
// trailingSignedRequest only a trailing_signature (over thinking + content).
// Each matrix case starts from the baseline whose SINGLE token makes the
// expected class unambiguous — the SDK's trailing binding also covers content,
// so a fully-signed baseline would name the earlier binding first.
func contentSignedRequest() *pb.ChatRequest {
	r := baseRequest()
	m := r.Messages[1]
	m.Content = "signed content"
	m.ContentSignature = "cs-token"
	return r
}

// unsignedContentRequest is the same content with no token at all — the
// baseline for the "token appeared" case, where minting one is forgery.
func unsignedContentRequest() *pb.ChatRequest {
	r := baseRequest()
	r.Messages[1].Content = "signed content"
	return r
}

func thinkingSignedRequest() *pb.ChatRequest {
	r := baseRequest()
	m := r.Messages[1]
	m.Thinking = "signed thinking"
	m.ThinkingSignature = "ts-token"
	m.RedactedThinking = "redacted"
	return r
}

func trailingSignedRequest() *pb.ChatRequest {
	r := baseRequest()
	m := r.Messages[1]
	m.Content = "signed content"
	m.Thinking = "signed thinking"
	m.TrailingSignature = "trailing-token"
	return r
}

// The bound-signature rule on the REQUEST path: there is no apply block to
// invalidate a wire token later, so SignatureStale is a violation here (it is
// tolerated on the response path). Clearing the token over changed content is
// the prescribed response and is accepted; dropping, forging or minting a
// token is provenance fraud and is rejected. Every subtest grants the message
// role, because the signature fields are also in the message fingerprint — the
// section check passes, and the binding check decides.
func TestVerifyRequestMutationSignatureMatrix(t *testing.T) {
	user := grant("ir.messages.write.user")

	cases := []struct {
		name string
		// base builds the accepted request; out starts from it.
		base  func() *pb.ChatRequest
		apply func(*pb.ChatRequest)
		// want is "" for accepted mutations.
		want string
	}{
		{name: "untouched", base: contentSignedRequest, want: ""},
		{name: "content changed token kept -> stale", base: contentSignedRequest,
			want:  "plugin content_signature signature stale",
			apply: func(r *pb.ChatRequest) { r.Messages[1].Content = "signed content'" }},
		{name: "content changed token cleared -> accepted", base: contentSignedRequest,
			apply: func(r *pb.ChatRequest) {
				r.Messages[1].Content = "signed content'"
				r.Messages[1].ContentSignature = ""
			}},
		{name: "token dropped without change -> dropped", base: contentSignedRequest,
			want:  "plugin content_signature signature dropped",
			apply: func(r *pb.ChatRequest) { r.Messages[1].ContentSignature = "" }},
		{name: "token forged -> forged", base: contentSignedRequest,
			want:  "plugin content_signature signature forged",
			apply: func(r *pb.ChatRequest) { r.Messages[1].ContentSignature = "evil" }},
		{name: "token added -> added", base: unsignedContentRequest,
			want: "plugin content_signature signature added",
			apply: func(r *pb.ChatRequest) {
				r.Messages[1].ContentSignature = "minted"
				r.Messages[1].Content = "signed content" // unchanged
			}},
		{name: "thinking changed token kept -> stale", base: thinkingSignedRequest,
			want:  "plugin thinking_signature signature stale",
			apply: func(r *pb.ChatRequest) { r.Messages[1].Thinking = "signed thinking'" }},
		{name: "redacted thinking changed token kept -> stale", base: thinkingSignedRequest,
			want:  "plugin thinking_signature signature stale",
			apply: func(r *pb.ChatRequest) { r.Messages[1].RedactedThinking = "redacted'" }},
		{name: "thinking changed token cleared -> accepted", base: thinkingSignedRequest,
			apply: func(r *pb.ChatRequest) {
				r.Messages[1].Thinking = "signed thinking'"
				r.Messages[1].ThinkingSignature = ""
			}},
		{name: "trailing content changed token kept -> stale", base: trailingSignedRequest,
			want:  "plugin trailing_signature signature stale",
			apply: func(r *pb.ChatRequest) { r.Messages[1].Content = "signed content'" }},
		{name: "trailing thinking changed token cleared -> accepted", base: trailingSignedRequest,
			apply: func(r *pb.ChatRequest) {
				r.Messages[1].Thinking = "signed thinking'"
				r.Messages[1].TrailingSignature = ""
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accepted := tc.base()
			out := tc.base()
			if tc.apply != nil {
				tc.apply(out)
			}

			err := verifyRequestMutation(accepted, out, user)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("mutation must be accepted, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("mutation must be rejected with %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("error = %q, want %q", err, tc.want)
			}
		})
	}
}

// The request-domain binding inventory is part of the enforcement contract: a
// token the SDK starts covering that this host does not know about would be a
// signature a plugin could forge unnoticed. outboundpolicy.Validate pins the
// shapes SDK-side; this pins what the verifier actually consumes, so a fourth
// binding fails loudly here instead of silently narrowing enforcement.
func TestRequestSignatureBindingsArePinned(t *testing.T) {
	got := make([]string, 0, len(requestSignatureFields))
	covered := map[string][]string{}
	for _, check := range requestSignatureFields {
		got = append(got, check.binding.SignatureField)
		for _, ref := range check.refs {
			covered[check.binding.SignatureField] = append(covered[check.binding.SignatureField], string(ref.Name()))
		}
	}

	wantFields := map[string]bool{
		"thinking_signature": true,
		"content_signature":  true,
		"trailing_signature": true,
	}
	if len(got) != len(wantFields) {
		t.Fatalf("request signature bindings = %v, want exactly %v", got, wantFields)
	}
	for _, f := range got {
		if !wantFields[f] {
			t.Errorf("unexpected request signature binding %q", f)
		}
	}
	// The pinned covered-content sets (mirrors the SDK's request contracts).
	wantCovered := map[string][]string{
		"thinking_signature": {"thinking", "redacted_thinking"},
		"content_signature":  {"content"},
		"trailing_signature": {"thinking", "content"},
	}
	for field, want := range wantCovered {
		if strings.Join(covered[field], ",") != strings.Join(want, ",") {
			t.Errorf("%s covers %v, want %v", field, covered[field], want)
		}
	}
}

func TestHoldsAllRequestGrants(t *testing.T) {
	all := fakeGrants(map[string]bool{})
	for _, g := range allRequestGrants {
		all[g] = true
	}
	if !holdsAllRequestGrants(all) {
		t.Fatal("a plugin holding every request grant must hit the fast path")
	}

	for _, missing := range allRequestGrants {
		partial := fakeGrants(map[string]bool{})
		for _, g := range allRequestGrants {
			if g != missing {
				partial[g] = true
			}
		}
		if holdsAllRequestGrants(partial) {
			t.Errorf("plugin missing %s was treated as fully granted", missing)
		}
	}

	if holdsAllRequestGrants(fakeGrants(nil)) {
		t.Fatal("a plugin with no grants must not hit the fast path")
	}
}

// ---------------------------------------------------------------------------
// Pipeline tests. test-mutator's manifest declares the full request write
// grant set, so an explicit approval can grant any SUBSET of it — including
// the empty set, which is how a grantless mutating plugin is exercised without
// a second wasm fixture. BundleDigestForDir is computed from disk, so the
// manifest change and the approval stay consistent.
// ---------------------------------------------------------------------------

func newApprovedPipeline(t *testing.T, name string, permissions []string, failureMode string) *PluginPipeline {
	t.Helper()
	digest, err := BundleDigestForDir(fixturesDir + "/" + name)
	if err != nil {
		t.Fatal(err)
	}
	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { rt.Close() })
	pp, err := NewPipeline(rt, PluginConfig{
		Dir:   fixturesDir,
		Order: []string{name},
		Approvals: map[string]Approval{
			name: {Digest: digest, Permissions: permissions, FailureMode: failureMode},
		},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if pp.Len() != 1 {
		t.Fatalf("loaded %d plugins, want 1", pp.Len())
	}
	return pp
}

// A plugin holding NO request grants that rewrites a message must have its
// replacement refused. Under allow mode there is no error and the accepted
// request chains onward unchanged.
func TestRunBeforeRequestGrantlessMutationRejectedAllow(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newApprovedPipeline(t, "test-mutator", nil, "pass")

	chat := &engine.ChatRequest{
		Model:    "gpt-x",
		Messages: []engine.Message{{Role: engine.RoleUser, Content: "hello"}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 1, chat)
	if err != nil {
		t.Fatalf("allow mode must not error on a refused replacement: %v", err)
	}
	if out == nil || len(out.Messages) != 1 || out.Messages[0].Content != "hello" {
		t.Fatalf("grantless mutation was applied: %+v", out.Messages)
	}
	if out.Model != "gpt-x" {
		t.Errorf("model changed: %q", out.Model)
	}
}

// The same grantless mutation under a block override is an attributed error
// naming the plugin, with the original request returned alongside it.
func TestRunBeforeRequestGrantlessMutationRejectedBlock(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newApprovedPipeline(t, "test-mutator", nil, "block")

	chat := &engine.ChatRequest{
		Model:    "gpt-x",
		Messages: []engine.Message{{Role: engine.RoleUser, Content: "hello"}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 2, chat)
	if err == nil {
		t.Fatal("block mode must return an error for a grantless mutation")
	}
	if !strings.Contains(err.Error(), "test-mutator") {
		t.Errorf("error %q does not name the offending plugin", err)
	}
	if !strings.Contains(err.Error(), "invalid request replacement") {
		t.Errorf("error %q does not describe the invalid replacement", err)
	}
	if !strings.Contains(err.Error(), "ir.messages.write.user") {
		t.Errorf("error %q does not name the missing grant", err)
	}
	if out != chat {
		t.Error("blocked call should return the original request unchanged")
	}
}

// A plugin granted exactly the sections it changes has its replacement applied.
func TestRunBeforeRequestGrantedMutationAccepted(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	pp := newApprovedPipeline(t, "test-mutator",
		[]string{"ir.messages.write.user", "ir.tools.write"}, "pass")

	chat := &engine.ChatRequest{
		Model:    "gpt-x",
		Messages: []engine.Message{{Role: engine.RoleUser, Content: "hello"}},
		Tools: []engine.ToolDef{{
			Name:       "read",
			Parameters: map[string]any{"type": "object"},
		}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 3, chat)
	if err != nil {
		t.Fatalf("a granted mutation must be accepted: %v", err)
	}
	if len(out.Messages) != 1 || !strings.HasSuffix(out.Messages[0].Content, "[seen by test-mutator]") {
		t.Errorf("granted message mutation was not applied: %+v", out.Messages)
	}
	if len(out.Tools) != 1 || out.Tools[0].Description != "described by test-mutator" {
		t.Errorf("granted tool mutation was not applied: %+v", out.Tools)
	}
}

// A plugin holding every request grant takes the fast path: verification is
// skipped entirely and the replacement is applied. The predicate is unit-tested
// above; this proves the fully-granted fixture drives the skip branch end to
// end. (Proving the skip is OBSERVABLE — a mutation that would fail
// verification but is accepted — would need a fixture that produces a
// verification violation while holding all grants, and the request-mutating
// fixtures cannot: adding one means a new wasm build, which the Makefile
// contract forbids.)
func TestRunBeforeRequestFullyGrantedFastPath(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-mutator/plugin.wasm")
	// AllowUnapproved converts manifest requests into grants: test-mutator
	// declares every request write grant, so the fast path applies.
	pp := newTestPipeline(t, fixturesDir, []string{"test-mutator"})

	chat := &engine.ChatRequest{
		Model:    "gpt-x",
		Messages: []engine.Message{{Role: engine.RoleUser, Content: "hello"}},
		Tools: []engine.ToolDef{{
			Name:       "read",
			Parameters: map[string]any{"type": "object"},
		}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 4, chat)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	if len(out.Messages) != 1 || !strings.HasSuffix(out.Messages[0].Content, "[seen by test-mutator]") {
		t.Errorf("fast-path mutation was not applied: %+v", out.Messages)
	}
	if len(out.Tools) != 1 || out.Tools[0].Description != "described by test-mutator" {
		t.Errorf("fast-path tool mutation was not applied: %+v", out.Tools)
	}
}

// --- benchmarks -------------------------------------------------------------

// BenchmarkVerifyRequestMutation is the per-plugin cost of the write-grant
// check when the plugin does NOT hold every request grant: decode the
// replacement, fingerprint both requests, compare every section, and run the
// request-domain signature bindings. The replacement is unmodified, which is
// the full-cost case — no section early-returns.
func BenchmarkVerifyRequestMutation(b *testing.B) {
	canWrite := grant("ir.messages.write.user", "ir.messages.write.assistant",
		"ir.messages.write.system", "ir.messages.write.tool",
		"ir.messages.write.developer", "ir.messages.write.other",
		"ir.tools.write", "ir.model.write", "ir.params.write")
	for _, n := range benchSizes {
		req := pbconv.ToPBChatRequest(benchConversation(n))
		raw, err := proto.Marshal(req)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(fmt.Sprintf("msgs=%d", n), func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var out pb.ChatRequest
				if err := proto.Unmarshal(raw, &out); err != nil {
					b.Fatal(err)
				}
				if err := verifyRequestMutation(req, &out, canWrite); err != nil {
					b.Fatal("unmodified request must verify clean")
				}
			}
		})
	}
}

// BenchmarkVerifyRequestMutationFastPath is what a plugin holding every request
// grant pays instead: one predicate over the grant set, no fingerprinting.
func BenchmarkVerifyRequestMutationFastPath(b *testing.B) {
	all := fakeGrants(map[string]bool{})
	for _, g := range allRequestGrants {
		all[g] = true
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !holdsAllRequestGrants(all) {
			b.Fatal("all grants must short-circuit")
		}
	}
}
