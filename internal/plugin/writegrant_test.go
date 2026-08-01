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
		base   func() *pb.ChatRequest
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
		// The only changed role is the unmodelled one, so the sorted union
		// names it and proves an unmodelled role maps to the catch-all
		// "other" grant. (Appending the message would mark every role, since
		// the message count is folded into each role's digest; mutating an
		// existing weird message isolates the role instead.)
		{name: "unmodelled-role", want: "plugin changed messages.weird without ir.messages.write.other",
			base: func() *pb.ChatRequest {
				r := baseRequest()
				r.Messages = append(r.Messages, &pb.Message{Role: "weird", Content: "W"})
				return r
			},
			mutate: func(r *pb.ChatRequest) { r.Messages[4].Content = "W'" }},
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

// --- F2: unconditional invariants (the all-grants fast-path checks) ----------

// verifyUnconditionalInvariants has no canWrite: grants authorise SECTIONS,
// never host facts or provenance, so these checks stand alone and the
// all-grants fast path still runs them.
func TestVerifyUnconditionalInvariantsHostOwnedMetaRejected(t *testing.T) {
	accepted := baseRequest()
	out := baseRequest()
	out.ToranaMetaJson = []byte(`{"_provider":"evil"}`)
	if err := verifyUnconditionalInvariants(accepted, out); err == nil {
		t.Fatal("a torana_meta_json change must fail the unconditional invariants")
	}
}

func TestVerifyUnconditionalInvariantsSignatureStaleRejected(t *testing.T) {
	accepted := contentSignedRequest()
	out := contentSignedRequest()
	out.Messages[1].Content = "signed content'" // token kept over changed content
	err := verifyUnconditionalInvariants(accepted, out)
	if err == nil {
		t.Fatal("a stale signature must fail the unconditional invariants")
	}
	if err.Error() != "plugin content_signature signature stale" {
		t.Errorf("error = %q, want the stale binding named", err)
	}
}

func TestVerifyUnconditionalInvariantsUnchangedPasses(t *testing.T) {
	req := contentSignedRequest()
	if err := verifyUnconditionalInvariants(req, req); err != nil {
		t.Fatalf("an unchanged request must pass the unconditional invariants, got: %v", err)
	}
}

// --- F3: identity-based signature alignment ---------------------------------

// The reviewer's exact reproduction: a granted deletion BEFORE a signed
// message shifts its index. Round-1 positional pairing compared the unchanged
// token against an unrelated message and classified it as forged; identity
// alignment pairs by (role, token value) and sees the token intact.
func TestVerifyRequestSignaturesDeletionBeforeSignedMessage(t *testing.T) {
	accepted := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "user", Content: "discard me"},
		{Role: "assistant", Content: "signed", ContentSignature: "token"},
	}}
	out := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "assistant", Content: "signed", ContentSignature: "token"},
	}}
	canWrite := grant("ir.messages.write.user", "ir.messages.write.assistant")
	if err := verifyRequestMutation(accepted, out, canWrite); err != nil {
		t.Fatalf("a granted deletion before a signed message must verify, got: %v", err)
	}
}

func TestVerifyRequestSignaturesInsertionBeforeSignedMessage(t *testing.T) {
	accepted := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "assistant", Content: "signed", ContentSignature: "token"},
	}}
	out := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "user", Content: "inserted"},
		{Role: "assistant", Content: "signed", ContentSignature: "token"},
	}}
	canWrite := grant("ir.messages.write.user", "ir.messages.write.assistant")
	if err := verifyRequestMutation(accepted, out, canWrite); err != nil {
		t.Fatalf("a granted insertion before a signed message must verify, got: %v", err)
	}
}

// Two signed messages of the same role swapped in full — tokens travel with
// their content, so both pair by token and stay intact.
func TestVerifyRequestSignaturesReorderSameRole(t *testing.T) {
	accepted := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "assistant", Content: "signed1", ContentSignature: "t1"},
		{Role: "assistant", Content: "signed2", ContentSignature: "t2"},
	}}
	out := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "assistant", Content: "signed2", ContentSignature: "t2"},
		{Role: "assistant", Content: "signed1", ContentSignature: "t1"},
	}}
	if err := verifyRequestMutation(accepted, out, grant("ir.messages.write.assistant")); err != nil {
		t.Fatalf("swapping two signed messages with their tokens must verify, got: %v", err)
	}
}

// The same swap WITHOUT the tokens travelling with their content must fail:
// alignment pairs by token and compares covered content, so a token moved
// onto different content is stale even under a reorder.
func TestVerifyRequestSignaturesReorderDetachesToken(t *testing.T) {
	accepted := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "assistant", Content: "signed1", ContentSignature: "t1"},
		{Role: "assistant", Content: "signed2", ContentSignature: "t2"},
	}}
	out := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "assistant", Content: "signed2", ContentSignature: "t1"},
		{Role: "assistant", Content: "signed1", ContentSignature: "t2"},
	}}
	err := verifyRequestMutation(accepted, out, grant("ir.messages.write.assistant"))
	if err == nil {
		t.Fatal("a token moved onto different covered content must be rejected")
	}
	if err.Error() != "plugin content_signature signature stale" {
		t.Errorf("error = %q, want the stale binding named", err)
	}
}

// Two accepted messages sharing one token over DIFFERENT content cannot be
// paired reliably — reject as ambiguous rather than guess.
func TestVerifyRequestSignaturesDuplicateTokenConflictingContent(t *testing.T) {
	accepted := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "user", Content: "A", ContentSignature: "t"},
		{Role: "user", Content: "B", ContentSignature: "t"},
	}}
	out := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "user", Content: "A", ContentSignature: "t"},
	}}
	err := verifyRequestMutation(accepted, out, grant("ir.messages.write.user"))
	if err == nil {
		t.Fatal("a duplicate token with conflicting content must be rejected as ambiguous")
	}
	if !strings.Contains(err.Error(), "duplicate token with conflicting content") {
		t.Errorf("error = %q, want the ambiguity named", err)
	}
}

// Candidates sharing both token AND content are the same signed fact — fine.
func TestVerifyRequestSignaturesDuplicateTokenIdenticalContent(t *testing.T) {
	accepted := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "user", Content: "A", ContentSignature: "t"},
		{Role: "user", Content: "A", ContentSignature: "t"},
	}}
	out := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "user", Content: "A", ContentSignature: "t"},
	}}
	if err := verifyRequestMutation(accepted, out, grant("ir.messages.write.user")); err != nil {
		t.Fatalf("duplicate tokens over identical content must verify, got: %v", err)
	}
}

// An accepted token with no output counterpart is allowed: deleting the
// signed message is a grantable deletion.
func TestVerifyRequestSignaturesAcceptedTokenWithoutCounterpartAllowed(t *testing.T) {
	accepted := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "user", Content: "signed", ContentSignature: "token"},
	}}
	out := &pb.ChatRequest{Messages: []*pb.Message{
		{Role: "assistant", Content: "fresh"},
	}}
	canWrite := grant("ir.messages.write.user", "ir.messages.write.assistant")
	if err := verifyRequestMutation(accepted, out, canWrite); err != nil {
		t.Fatalf("deleting a signed message must verify, got: %v", err)
	}
}

// --- F6: the changed-role union is checked in sorted order ------------------

// Two roles change and neither is granted; the lexicographically first role
// must be named, deterministically, no matter which map iteration order the
// fingerprint comparison visits first.
func TestVerifyGrantedSectionsReportsSortedRoleFirst(t *testing.T) {
	accepted := baseRequest()
	out := baseRequest()
	out.Messages[1].Content = "A'"                                 // user
	out.Messages[2].ToolCalls[0].ArgumentsJson = []byte(`{"p":2}`) // assistant

	err := verifyRequestMutation(accepted, out, grant())
	if err == nil {
		t.Fatal("an ungranted change must be rejected")
	}
	if err.Error() != "plugin changed messages.assistant without ir.messages.write.assistant" {
		t.Errorf("error = %q, want the lexicographically first changed role named", err)
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
// Pipeline tests. All-or-nothing approval is settled: a plugin is granted
// either everything its manifest declares or nothing. test-mutator's manifest
// declares the full request write set, so a pipeline approval here is either
// the empty set (a grantless mutating plugin — rejection fixtures) or the
// full set (the all-grants fast path). Intermediate grant sets are exercised
// at the UNIT level with canWrite closures above, never by faking a partial
// approval. Fixtures whose manifests declare exactly the grants they need
// (test-forge-host-meta, test-stale-bind: the full write set; test-mutator:
// the full write set) hit the fast path; test-verdict-then-invalid declares
// only its verdict permissions and is therefore grantless for writes.
// BundleDigestForDir is computed from disk, so the manifest change and the
// approval stay consistent.
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

// A plugin holding every request grant takes the fast path: the section
// comparison is skipped and the replacement is applied — but the unconditional
// invariants still run, so the replacement must not touch host-owned fields or
// signatures (proven end to end by TestRunBeforeRequestAllGrants* below). The
// predicate is unit-tested above; this proves the fully-granted fixture drives
// the skip branch end to end with a clean replacement.
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

// --- F2 end to end: the all-grants fast path still checks host facts --------

// test-forge-host-meta declares the full request write set, so it hits the
// fast path — and its replacement forges host-owned torana_meta_json, which no
// grant authorises. The replacement must be refused even though the plugin
// can write every section.
func TestRunBeforeRequestAllGrantsHostOwnedMetaRejectedAllow(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-forge-host-meta/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-forge-host-meta"})

	chat := &engine.ChatRequest{
		Model:    "gpt-x",
		Messages: []engine.Message{{Role: engine.RoleUser, Content: "hello"}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 1, chat)
	if err != nil {
		t.Fatalf("allow mode must not error on a refused replacement: %v", err)
	}
	if out == nil || len(out.Messages) != 1 || out.Messages[0].Content != "hello" {
		t.Fatalf("the forged-metadata replacement leaked into the request: %+v", out.Messages)
	}
	if len(out.ToranaMeta) != 0 {
		t.Errorf("host-owned metadata was changed on the fast path: %v", out.ToranaMeta)
	}
}

// The same refusal under a block override is an attributed error naming the
// plugin, with the original request returned alongside it.
func TestRunBeforeRequestAllGrantsHostOwnedMetaRejectedBlock(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-forge-host-meta/plugin.wasm")
	pp := newApprovedPipeline(t, "test-forge-host-meta", allRequestGrants, "block")

	chat := &engine.ChatRequest{
		Model:    "gpt-x",
		Messages: []engine.Message{{Role: engine.RoleUser, Content: "hello"}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 2, chat)
	if err == nil {
		t.Fatal("block mode must return an error for a host-owned violation on the fast path")
	}
	if !strings.Contains(err.Error(), "test-forge-host-meta") {
		t.Errorf("error %q does not name the offending plugin", err)
	}
	if !strings.Contains(err.Error(), "invalid request replacement") {
		t.Errorf("error %q does not describe the invalid replacement", err)
	}
	if !strings.Contains(err.Error(), "host-owned torana_meta_json") {
		t.Errorf("error %q does not name the host-owned violation", err)
	}
	if out != chat {
		t.Error("blocked call should return the original request unchanged")
	}
}

// test-stale-bind declares the full request write set (fast path) and changes
// the content a content_signature covers while KEEPING the token. On the
// request path there is no apply block to invalidate the wire token later, so
// the stale binding is a violation even for an all-grants plugin.
func TestRunBeforeRequestAllGrantsStaleBindRejectedAllow(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-stale-bind/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-stale-bind"})

	chat := &engine.ChatRequest{
		Model: "gpt-x",
		Messages: []engine.Message{
			{Role: engine.RoleUser, Content: "signed content", ContentSignature: "cs-token"},
		},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 3, chat)
	if err != nil {
		t.Fatalf("allow mode must not error on a refused replacement: %v", err)
	}
	if out == nil || len(out.Messages) != 1 || out.Messages[0].Content != "signed content" {
		t.Fatalf("the stale-bound replacement leaked into the request: %+v", out.Messages)
	}
	if out.Messages[0].ContentSignature != "cs-token" {
		t.Errorf("content_signature changed: %q", out.Messages[0].ContentSignature)
	}
}

// --- F4: a refused replacement discards trapped verdicts --------------------

// A respond verdict recorded before a grantless replacement must be discarded
// when verification refuses the replacement: the refusal is a trap-equivalent
// exit, and a half-built synthetic response from code that then produced an
// invalid replacement is not trustworthy. (failure_mode is pass, so the only
// reason the respond is gone is the discard.)
func TestRunBeforeRequestRejectedReplacementDiscardsRespond(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-verdict-then-invalid/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-verdict-then-invalid"})

	const reqID = 5
	chat := &engine.ChatRequest{
		Model:    "gpt-x",
		Messages: []engine.Message{{Role: engine.RoleUser, Content: "respondme hello"}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), reqID, chat)
	if err != nil {
		t.Fatalf("failure_mode is pass, so a refused replacement must not error: %v", err)
	}
	v := pp.Verdicts(reqID)
	if v == nil || v.Respond() != nil {
		t.Fatal("a respond verdict recorded before an invalid replacement survived — " +
			"the refusal must discard it like a trap")
	}
	if v.Block() != nil {
		t.Fatal("no block was recorded in this case")
	}
	if out == nil || len(out.Messages) != 1 || out.Messages[0].Content != "respondme hello" {
		t.Fatalf("the grantless replacement was applied: %+v", out.Messages)
	}
}

// A block recorded before the invalid replacement must FAIL CLOSED: the
// discard drops respond/route/identity but never a block, and the block still
// short-circuits the pipeline.
func TestRunBeforeRequestRejectedReplacementKeepsBlock(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-verdict-then-invalid/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-verdict-then-invalid"})

	const reqID = 6
	chat := &engine.ChatRequest{
		Model:    "gpt-x",
		Messages: []engine.Message{{Role: engine.RoleUser, Content: "blockme hello"}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), reqID, chat)
	if err != nil {
		t.Fatalf("failure_mode is pass, so a refused replacement must not error: %v", err)
	}
	v := pp.Verdicts(reqID)
	if v == nil || v.Block() == nil {
		t.Fatal("a block recorded before an invalid replacement must fail closed")
	}
	if got := v.Block().Code; got != "blocked_then_invalid" {
		t.Errorf("block code = %q, want the one the guest recorded", got)
	}
	if v.Respond() != nil {
		t.Error("a respond verdict from the same refused call was kept")
	}
	if out == nil || len(out.Messages) != 1 || out.Messages[0].Content != "blockme hello" {
		t.Fatalf("the grantless replacement was applied: %+v", out.Messages)
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
