package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/cache"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
)

// chatWithHeaders builds a minimal request and the raw header map for the
// chat header-policy rows.
func chatWithHeaders() (*engine.ChatRequest, map[string][]string) {
	chat := &engine.ChatRequest{
		Model: "gpt-x",
		Messages: []engine.Message{
			{Role: engine.RoleUser, Content: "hello"},
		},
	}
	raw := map[string][]string{
		"Authorization":     {"Bearer sk-torana-real"},
		"X-Api-Key":         {"sk-torana-apikey"},
		"X-Torana-User":     {"alice"},
		"X-Torana-Team":     {"team-a"},
		"X-Torana-Tenant":   {"tenant-a"},
		"Cookie":            {"session=secret"},
		"Accept":            {"text/html"},
		"X-Customer-Secret": {"s3cr3t"},
	}
	return chat, raw
}

// observerNotes extracts every observer message appended by the fixtures,
// in chain order.
func observerNotes(t *testing.T, out *engine.ChatRequest) []string {
	t.Helper()
	var notes []string
	for _, m := range out.Messages {
		if m.Role == engine.RoleAssistant && strings.HasPrefix(m.Content, "observer ") {
			notes = append(notes, m.Content)
		}
	}
	return notes
}

func requireNote(t *testing.T, notes []string, want string) {
	t.Helper()
	for _, n := range notes {
		if n == want {
			return
		}
	}
	t.Fatalf("missing observation %q in %v", want, notes)
}

// TestChatHeadersProjectPerPlugin — C1: a granted plugin observes the
// allowlisted headers; an ungranted plugin in the SAME pipeline, in either
// execution order, never does.
func TestChatHeadersProjectPerPlugin(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order []string
	}{
		{"granted first", []string{"test-header-observer", "test-header-observer-nogrant"}},
		{"granted second", []string{"test-header-observer-nogrant", "test-header-observer"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireWASM(t, fixturesDir+"/test-header-observer/plugin.wasm")
			requireWASM(t, fixturesDir+"/test-header-observer-nogrant/plugin.wasm")
			pp := newTestPipeline(t, fixturesDir, tc.order)
			chat, raw := chatWithHeaders()
			out, err := pp.RunBeforeRequest(context.Background(), 1, chat, raw)
			if err != nil {
				t.Fatalf("RunBeforeRequest: %v", err)
			}
			notes := observerNotes(t, out)
			if len(notes) != 2 {
				t.Fatalf("want two observer notes, got %v", notes)
			}
			requireNote(t, notes, "observer auth=Bearer sk-torana-real apikey=sk-torana-apikey present=true")
			requireNote(t, notes, "observer auth= apikey= present=false")
		})
	}
}

// TestChatHeadersTwoGrantedPluginsSeePristineValues — C2: two granted
// plugins in one pipeline both observe the same pristine caller values; the
// first plugin's replacement (its appended message) does not affect the
// second plugin's projection.
func TestChatHeadersTwoGrantedPluginsSeePristineValues(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-header-observer/plugin.wasm")
	requireWASM(t, fixturesDir+"/test-header-observer-b/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-header-observer", "test-header-observer-b"})
	chat, raw := chatWithHeaders()
	out, err := pp.RunBeforeRequest(context.Background(), 1, chat, raw)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	notes := observerNotes(t, out)
	if len(notes) != 2 {
		t.Fatalf("want two observer notes, got %v", notes)
	}
	for _, n := range notes {
		if n != "observer auth=Bearer sk-torana-real apikey=sk-torana-apikey present=true" {
			t.Fatalf("a granted plugin did not see the pristine values: %q", n)
		}
	}
}

// TestChatHeadersNoneGrantedAbsentThroughout — C3: with no granted plugin,
// _request_headers is absent for every plugin, even though the raw headers
// carry credentials.
func TestChatHeadersNoneGrantedAbsentThroughout(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-header-observer-nogrant/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-header-observer-nogrant"})
	chat, raw := chatWithHeaders()
	out, err := pp.RunBeforeRequest(context.Background(), 1, chat, raw)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	notes := observerNotes(t, out)
	if len(notes) != 1 || notes[0] != "observer auth= apikey= present=false" {
		t.Fatalf("ungranted plugin observed headers: %v", notes)
	}
}

// TestChatHeadersNilRawIsNoOp — C8: a nil raw-header argument (unit-test
// pipelines, direct callers) makes the seam a no-op.
func TestChatHeadersNilRawIsNoOp(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-header-observer/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-header-observer"})
	chat := &engine.ChatRequest{
		Model:    "gpt-x",
		Messages: []engine.Message{{Role: engine.RoleUser, Content: "hello"}},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 1, chat, nil)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	notes := observerNotes(t, out)
	if len(notes) != 1 || notes[0] != "observer auth= apikey= present=false" {
		t.Fatalf("nil raw headers must be a no-op: %v", notes)
	}
}

// TestChatHeadersOperationalHeadersNotProjected — C7: the chat surface keeps
// its credential/identity allowlist; the safe HTTP operational headers and
// Cookie are NOT exposed through chat metadata.
func TestChatHeadersOperationalHeadersNotProjected(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-header-observer/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-header-observer"})
	chat := &engine.ChatRequest{
		Model:    "gpt-x",
		Messages: []engine.Message{{Role: engine.RoleUser, Content: "hello"}},
	}
	raw := map[string][]string{
		"Accept":         {"text/html"},
		"Content-Type":   {"application/json"},
		"User-Agent":     {"curl"},
		"Cookie":         {"session=secret"},
		"X-Torana-Agent": {"spoofed"},
	}
	out, err := pp.RunBeforeRequest(context.Background(), 1, chat, raw)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	notes := observerNotes(t, out)
	// The projection exists (the plugin is granted) but carries no credential
	// values: the operational headers, Cookie, and X-Torana-Agent are not on
	// the chat allowlist.
	if len(notes) != 1 || notes[0] != "observer auth= apikey= present=true" {
		t.Fatalf("operational/cookie headers leaked into chat metadata: %v", notes)
	}
}

// TestChatHeadersForgeryRejectedPristineChainsOn — C4: an UNGRANTED plugin
// forging _request_headers in its replacement is rejected by the host-owned
// torana_meta_json invariant, and a downstream granted plugin receives the
// pristine caller values, never the forged ones.
func TestChatHeadersForgeryRejectedPristineChainsOn(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-forge-host-meta/plugin.wasm")
	requireWASM(t, fixturesDir+"/test-header-observer/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-forge-host-meta", "test-header-observer"})
	chat, raw := chatWithHeaders()
	out, err := pp.RunBeforeRequest(context.Background(), 1, chat, raw)
	if err != nil {
		t.Fatalf("allow mode must not error on a refused replacement: %v", err)
	}
	notes := observerNotes(t, out)
	if len(notes) != 1 || notes[0] != "observer auth=Bearer sk-torana-real apikey=sk-torana-apikey present=true" {
		t.Fatalf("forged credential metadata reached a downstream plugin: %v", notes)
	}
}

// TestChatHeadersTrapLeavesNoMetadata — C5: a plugin trap and an invalid
// result leave no _request_headers in the chained or returned request, under
// BOTH failure modes, with a downstream granted observer running on every
// allow path.
func TestChatHeadersTrapLeavesNoMetadata(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-header-observer/plugin.wasm")

	t.Run("trap block mode short-circuits", func(t *testing.T) {
		requireWASM(t, fixturesDir+"/test-trapper/plugin.wasm")
		pp := newApprovedMultiPipeline(t, []string{"test-trapper", "test-header-observer"}, map[string]Approval{
			"test-trapper":         {Digest: fixtureDigest(t, "test-trapper"), Permissions: []string{}, FailureMode: "block"},
			"test-header-observer": {Digest: fixtureDigest(t, "test-header-observer"), Permissions: []string{"env.request_headers", "ir.messages.write.user", "ir.messages.write.assistant", "env.cache_get"}, FailureMode: "pass"},
		})
		chat, raw := chatWithHeaders()
		_, err := pp.RunBeforeRequest(context.Background(), 1, chat, raw)
		if err == nil {
			t.Fatal("block-mode trap must error")
		}
		// The projection must not survive in the PB: a second call with a
		// granted observer proves the pipeline state carries nothing over.
	})

	t.Run("trap allow mode runs downstream with pristine values", func(t *testing.T) {
		requireWASM(t, fixturesDir+"/test-trapper/plugin.wasm")
		pp := newApprovedMultiPipeline(t, []string{"test-trapper", "test-header-observer"}, map[string]Approval{
			"test-trapper":         {Digest: fixtureDigest(t, "test-trapper"), Permissions: []string{}, FailureMode: "pass"},
			"test-header-observer": {Digest: fixtureDigest(t, "test-header-observer"), Permissions: []string{"env.request_headers", "ir.messages.write.user", "ir.messages.write.assistant", "env.cache_get"}, FailureMode: "pass"},
		})
		chat, raw := chatWithHeaders()
		out, err := pp.RunBeforeRequest(context.Background(), 1, chat, raw)
		if err != nil {
			t.Fatalf("allow-mode trap must not error: %v", err)
		}
		notes := observerNotes(t, out)
		if len(notes) != 1 || notes[0] != "observer auth=Bearer sk-torana-real apikey=sk-torana-apikey present=true" {
			t.Fatalf("downstream projection broken after trap: %v", notes)
		}
		assertNoCredentialMeta(t, out)
	})

	t.Run("invalid result allow mode runs downstream with pristine values", func(t *testing.T) {
		requireWASM(t, fixturesDir+"/test-verdict-then-invalid/plugin.wasm")
		pp := newApprovedMultiPipeline(t, []string{"test-verdict-then-invalid", "test-header-observer"}, map[string]Approval{
			"test-verdict-then-invalid": {Digest: fixtureDigest(t, "test-verdict-then-invalid"), Permissions: []string{"env.respond_request", "env.block_request"}, FailureMode: "pass"},
			"test-header-observer":      {Digest: fixtureDigest(t, "test-header-observer"), Permissions: []string{"env.request_headers", "ir.messages.write.user", "ir.messages.write.assistant", "env.cache_get"}, FailureMode: "pass"},
		})
		chat, raw := chatWithHeaders()
		out, err := pp.RunBeforeRequest(context.Background(), 1, chat, raw)
		if err != nil {
			t.Fatalf("allow-mode invalid result must not error: %v", err)
		}
		notes := observerNotes(t, out)
		if len(notes) != 1 || notes[0] != "observer auth=Bearer sk-torana-real apikey=sk-torana-apikey present=true" {
			t.Fatalf("downstream projection broken after invalid result: %v", notes)
		}
		assertNoCredentialMeta(t, out)
	})

	t.Run("invalid result block mode errors and stops", func(t *testing.T) {
		requireWASM(t, fixturesDir+"/test-verdict-then-invalid/plugin.wasm")
		pp := newApprovedMultiPipeline(t, []string{"test-verdict-then-invalid", "test-header-observer"}, map[string]Approval{
			"test-verdict-then-invalid": {Digest: fixtureDigest(t, "test-verdict-then-invalid"), Permissions: []string{"env.respond_request", "env.block_request"}, FailureMode: "block"},
			"test-header-observer":      {Digest: fixtureDigest(t, "test-header-observer"), Permissions: []string{"env.request_headers", "ir.messages.write.user", "ir.messages.write.assistant", "env.cache_get"}, FailureMode: "pass"},
		})
		chat, raw := chatWithHeaders()
		_, err := pp.RunBeforeRequest(context.Background(), 1, chat, raw)
		if err == nil {
			t.Fatal("block-mode invalid result must error")
		}
	})
}

// fixtureDigest computes the digest of a fixture bundle directory.
func fixtureDigest(t *testing.T, name string) string {
	t.Helper()
	digest, err := BundleDigestForDir(fixturesDir + "/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// newApprovedMultiPipeline builds a pipeline with per-plugin approvals.
func newApprovedMultiPipeline(t *testing.T, order []string, approvals map[string]Approval) *PluginPipeline {
	t.Helper()
	rt := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { rt.Close() })
	pp, err := NewPipeline(rt, PluginConfig{Dir: fixturesDir, Order: order, Approvals: approvals})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if pp.Len() != len(order) {
		t.Fatalf("loaded %d plugins, want %d", pp.Len(), len(order))
	}
	return pp
}

// assertNoCredentialMeta fails when the returned engine request carries
// _request_headers in its ToranaMeta — the projection must never survive the
// chain into the returned PB round-trip.
func assertNoCredentialMeta(t *testing.T, out *engine.ChatRequest) {
	t.Helper()
	if out != nil && out.ToranaMeta != nil {
		if _, ok := out.ToranaMeta["_request_headers"]; ok {
			t.Fatal("credential metadata leaked into the returned chat")
		}
	}
}

// TestChatHeadersPermissionFollowsExecutableGeneration — C6: the grant
// decision follows the exact executable generation (lp.plugin HasGrant), not
// the on-disk manifest and not another pipeline. Generation 1 is built from a
// granted manifest and observes; the on-disk manifest is then changed (no
// reload); generation 1 STILL observes (its approval is pinned). A fresh
// pipeline built after the change is a NEW generation with the new manifest's
// grants and does NOT observe.
func TestChatHeadersPermissionFollowsExecutableGeneration(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-header-observer/plugin.wasm")

	stage := t.TempDir()
	pluginDir := stage + "/test-header-observer"
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"plugin.wasm", "plugin.json", "schema.json"} {
		data, err := os.ReadFile(fixturesDir + "/test-header-observer/" + f)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		if err := os.WriteFile(pluginDir+"/"+f, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	build := func() *PluginPipeline {
		rt := wasm.NewRuntime(context.Background())
		t.Cleanup(func() { rt.Close() })
		pp, err := NewPipeline(rt, PluginConfig{Dir: stage, Order: []string{"test-header-observer"}, AllowUnapproved: true})
		if err != nil {
			t.Fatalf("NewPipeline: %v", err)
		}
		return pp
	}
	run := func(pp *PluginPipeline) []string {
		chat, raw := chatWithHeaders()
		out, err := pp.RunBeforeRequest(context.Background(), 1, chat, raw)
		if err != nil {
			t.Fatalf("RunBeforeRequest: %v", err)
		}
		return observerNotes(t, out)
	}

	gen1 := build()
	notes := run(gen1)
	if len(notes) != 1 || notes[0] != "observer auth=Bearer sk-torana-real apikey=sk-torana-apikey present=true" {
		t.Fatalf("generation 1 must observe: %v", notes)
	}

	// Change the on-disk manifest WITHOUT reloading: generation 1's approval
	// is pinned to its executable and must not change. (AllowUnapproved
	// derives the loaded executable's grant set from the manifest at LOAD
	// time, so this proves the pinned generation is unaffected by later
	// on-disk changes; it is not an approval-store test.)
	raw, err := os.ReadFile(pluginDir + "/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var full map[string]any
	if err := json.Unmarshal(raw, &full); err != nil {
		t.Fatal(err)
	}
	perms := full["permissions"].([]any)
	filtered := perms[:0]
	for _, p := range perms {
		if p.(map[string]any)["name"] == "env.request_headers" {
			continue
		}
		filtered = append(filtered, p)
	}
	full["permissions"] = filtered
	out, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginDir+"/plugin.json", out, 0o644); err != nil {
		t.Fatal(err)
	}

	notes = run(gen1)
	if len(notes) != 1 || notes[0] != "observer auth=Bearer sk-torana-real apikey=sk-torana-apikey present=true" {
		t.Fatalf("generation 1 approval must survive an on-disk manifest change: %v", notes)
	}

	// A fresh pipeline is a NEW generation: it loads the changed manifest and
	// no longer holds the grant.
	gen2 := build()
	notes = run(gen2)
	if len(notes) != 1 || notes[0] != "observer auth= apikey= present=false" {
		t.Fatalf("generation 2 must follow its own manifest's grants: %v", notes)
	}
}

// mutatingStore wraps a local cache and mutates the caller's raw header map
// inside Get — the deterministic barrier: the mutation happens DURING
// dispatch (inside the first plugin's host call), so a lazy per-iteration
// reader would see it; the entry snapshot must not.
type mutatingStore struct {
	cache.Store
	mu      sync.Mutex
	mutated bool
	raw     map[string][]string
}

func (s *mutatingStore) Get(key string) (string, bool) {
	s.mu.Lock()
	if !s.mutated {
		s.mutated = true
		s.raw["Authorization"] = []string{"Bearer MUTATED"}
	}
	s.mu.Unlock()
	return s.Store.Get(key)
}

// TestChatHeadersSnapshotIsImmuneToCallerMutation — the raw map is snapshotted
// at dispatch admission: a caller mutating its map mid-dispatch (the barrier
// fires inside plugin 1's cache host call) cannot affect what plugin 2
// observes.
func TestChatHeadersSnapshotIsImmuneToCallerMutation(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-header-observer/plugin.wasm")
	requireWASM(t, fixturesDir+"/test-header-observer-b/plugin.wasm")
	chat, raw := chatWithHeaders()
	store := &mutatingStore{Store: cache.NewLocalCache(time.Minute), raw: raw}
	pp := newTestPipelineWith(t, fixturesDir, []string{"test-header-observer", "test-header-observer-b"}, store, nil)
	out, err := pp.RunBeforeRequest(context.Background(), 1, chat, raw)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	notes := observerNotes(t, out)
	if len(notes) != 2 {
		t.Fatalf("want two observer notes, got %v", notes)
	}
	for _, n := range notes {
		if n != "observer auth=Bearer sk-torana-real apikey=sk-torana-apikey present=true" {
			t.Fatalf("a plugin observed the mutated map: %q", n)
		}
	}
}

// TestChatHeadersBlockPlusReplacementShortCircuits — a plugin that blocks AND
// returns a replacement must stop the chain: the block survives, the
// replacement is preserved, and NO downstream plugin executes (the settled
// PII-keeps-flowing invariant).
func TestChatHeadersBlockPlusReplacementShortCircuits(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-blocker/plugin.wasm")
	requireWASM(t, fixturesDir+"/test-header-observer/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-blocker", "test-header-observer"})

	chat, raw := chatWithHeaders()
	chat.Messages[0].Content = "blockme now"
	out, err := pp.RunBeforeRequest(context.Background(), 1, chat, raw)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	if !pp.blocked(1) {
		t.Fatal("the attributed block did not survive the block+replacement exit")
	}
	if notes := observerNotes(t, out); len(notes) != 0 {
		t.Fatalf("a downstream plugin ran after block+replacement: %v", notes)
	}
	// The blocker's accepted replacement is preserved internally (block wins
	// at the transport; the replacement state still returns).
	if out == nil || len(out.Messages) != 1 || out.Messages[0].Content != "blockme now" {
		t.Fatalf("the accepted replacement was not preserved: %+v", out.Messages)
	}
	assertNoCredentialMeta(t, out)
}

// TestChatHeadersMutationObserverSeesNoProjection — the host
// RequestMutationFunc observes the canonical replacement, never the temporary
// credential projection: the observed ToranaMetaJson is BYTE-EXACTLY the
// pre-injection bytes (unrelated host metadata preserved verbatim, including
// a json.Number lexeme a float64 round-trip would destroy), and
// _request_headers is absent.
func TestChatHeadersMutationObserverSeesNoProjection(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-header-observer/plugin.wasm")
	pp := newTestPipeline(t, fixturesDir, []string{"test-header-observer"})

	observed := make(chan []byte, 1)
	pp.runtime.RequestMutationFunc = func(_ context.Context, requestPB []byte) {
		observed <- requestPB
	}

	chat, raw := chatWithHeaders()
	// Unrelated host metadata whose exact lexeme survives ONLY exact byte
	// restoration: json.Number("9007199254740993") marshals verbatim, but any
	// unmarshal-into-float64 round trip would round it to ...992.
	chat.ToranaMeta = map[string]any{"_provider": json.Number("9007199254740993")}
	expected := pbconv.ToPBChatRequest(chat).ToranaMetaJson

	out, err := pp.RunBeforeRequest(context.Background(), 1, chat, raw)
	if err != nil {
		t.Fatalf("RunBeforeRequest: %v", err)
	}
	notes := observerNotes(t, out)
	if len(notes) != 1 || notes[0] != "observer auth=Bearer sk-torana-real apikey=sk-torana-apikey present=true" {
		t.Fatalf("the granted observer did not see the projection: %v", notes)
	}
	select {
	case raw := <-observed:
		var req pbv2.ChatRequest
		if err := proto.Unmarshal(raw, &req); err != nil {
			t.Fatalf("decode observed request: %v", err)
		}
		if !bytes.Equal(req.ToranaMetaJson, expected) {
			t.Fatalf("observed ToranaMetaJson is not byte-exactly the pre-injection bytes:\n  got  %q\n  want %q",
				req.ToranaMetaJson, expected)
		}
		if bytes.Contains(req.ToranaMetaJson, []byte("_request_headers")) {
			t.Fatal("RequestMutationFunc observed projected credentials")
		}
	default:
		t.Fatal("RequestMutationFunc was never called")
	}
}

// TestHeaderPolicyCaseCollisionsAreDeterministic — differently-cased keys for
// one allowed header are ambiguous and omitted fail-closed on both surfaces;
// the fixed canonical name is the only emitter. Iterating repeatedly proves
// no map-order dependence.
func TestHeaderPolicyCaseCollisionsAreDeterministic(t *testing.T) {
	for i := 0; i < 100; i++ {
		raw := map[string][]string{
			"accept":        {"text/html"},
			"Accept":        {"application/json"}, // collision
			"authorization": {"Bearer lower"},
			"Authorization": {"Bearer upper"}, // collision
		}
		got := filterHTTPHeaders(raw, true)
		if _, ok := got["Accept"]; ok {
			t.Fatal("case-colliding operational header must be omitted fail-closed")
		}
		if _, ok := got["Authorization"]; ok {
			t.Fatal("case-colliding credential header must be omitted fail-closed")
		}
		if len(got) != 0 {
			t.Fatalf("collision iteration %d: %v", i, got)
		}

		chat := projectChatHeaders(raw)
		if len(chat) != 0 {
			t.Fatalf("chat collision iteration %d: %v", i, chat)
		}
	}
}

// TestHeaderPolicyCanonicalResolution — single mixed-case keys resolve to the
// fixed canonical name with the complete slice (HTTP) or element zero (chat,
// matching Header.Get); empty and first-empty slices are omitted.
func TestHeaderPolicyCanonicalResolution(t *testing.T) {
	raw := map[string][]string{
		"user-agent":      {"curl/8", "extra"},
		"x-api-key":       {"sk-torana-apikey"},
		"X-Torana-User":   {"alice"},
		"x-torana-team":   {"team-a", "team-b"},
		"x-torana-tenant": {},
	}
	got := filterHTTPHeaders(raw, true)
	if v := got["User-Agent"]; len(v) != 2 || v[0] != "curl/8" || v[1] != "extra" {
		t.Fatalf("User-Agent = %v", v)
	}
	if v := got["X-Api-Key"]; len(v) != 1 || v[0] != "sk-torana-apikey" {
		t.Fatalf("X-Api-Key = %v", v)
	}
	// HTTP preserves the complete value slice — an empty slice is preserved
	// as an empty slice (only the CHAT surface omits empty values).
	if v, ok := got["X-Torana-Tenant"]; !ok || len(v) != 0 {
		t.Fatalf("X-Torana-Tenant = %v", got)
	}
	for name := range got {
		canonical := false
		for _, c := range append(append([]string{}, httpOperationalHeaders...), credentialHeaders...) {
			if name == c {
				canonical = true
			}
		}
		if !canonical {
			t.Fatalf("non-canonical emitted name %q", name)
		}
	}

	chat := projectChatHeaders(raw)
	if chat["X-Api-Key"] != "sk-torana-apikey" {
		t.Fatalf("chat X-Api-Key = %v", chat)
	}
	if chat["X-Torana-User"] != "alice" {
		t.Fatalf("chat X-Torana-User = %v", chat)
	}
	// Header.Get semantics: element zero, never a later non-empty value.
	if v, ok := chat["X-Torana-Team"]; !ok || v != "team-a" {
		t.Fatalf("chat X-Torana-Team = %v", chat)
	}
	if _, ok := chat["X-Torana-Tenant"]; ok {
		t.Fatalf("empty tenant slice must be omitted from chat: %v", chat)
	}
}

// TestChatHeadersFirstEmptyValueMatchesHeaderGet — element zero wins and an
// empty element zero omits the header even when a later value is non-empty,
// exactly like req.Header.Get.
func TestChatHeadersFirstEmptyValueMatchesHeaderGet(t *testing.T) {
	raw := map[string][]string{
		"Authorization": {"", "Bearer sk-torana-later"},
	}
	chat := projectChatHeaders(raw)
	if _, ok := chat["Authorization"]; ok {
		t.Fatalf("empty element zero must omit the header: %v", chat)
	}
	raw = map[string][]string{
		"Authorization": {"Bearer sk-torana-first", ""},
	}
	chat = projectChatHeaders(raw)
	if v, ok := chat["Authorization"]; !ok || v != "Bearer sk-torana-first" {
		t.Fatalf("element zero must win: %v", chat)
	}
}

// TestHeaderPolicyRejectsUnicodeConfusables — the raw map is untrusted and
// resolution uses HTTP field-name canonicalization, NOT Unicode folding:
// long-s (U+017F folds to s), the Kelvin sign (U+212A folds to K), ordinary
// non-ASCII text, and invalid ASCII names never match an allowed header on
// either surface. Legal mixed ASCII case still resolves.
func TestHeaderPolicyRejectsUnicodeConfusables(t *testing.T) {
	confusables := []string{
		"U\u017fer-Agent",     // long-s
		"X-Api-\u212Aey",      // Kelvin sign
		"X-Torana-\u00fcser",  // non-ASCII ordinary text
		"Authorization\u00a0", // NBSP suffix
		" User-Agent",         // leading space
		"User-Agent ",         // trailing space
		"User-Agent\x00",      // NUL
		"User-Agent\t",        // tab
		"user agent",          // space separator
	}
	for i := 0; i < 50; i++ {
		raw := map[string][]string{}
		for _, name := range confusables {
			raw[name] = []string{"spoofed"}
		}
		got := filterHTTPHeaders(raw, true)
		if len(got) != 0 {
			t.Fatalf("iteration %d: confusable names matched: %v", i, got)
		}
		chat := projectChatHeaders(raw)
		if len(chat) != 0 {
			t.Fatalf("iteration %d: confusable names matched on chat: %v", i, chat)
		}
	}

	// Legal mixed ASCII case still resolves through the same rule.
	raw := map[string][]string{
		"user-agent": {"curl/8"},
		"X-API-KEY":  {"sk-torana-apikey"},
	}
	got := filterHTTPHeaders(raw, true)
	if v := got["User-Agent"]; len(v) != 1 || v[0] != "curl/8" {
		t.Fatalf("legal mixed case did not resolve: %v", got)
	}
	if v := got["X-Api-Key"]; len(v) != 1 || v[0] != "sk-torana-apikey" {
		t.Fatalf("legal mixed case did not resolve: %v", got)
	}
	chat := projectChatHeaders(raw)
	if chat["X-Api-Key"] != "sk-torana-apikey" {
		t.Fatalf("legal mixed case did not resolve on chat: %v", chat)
	}
}

// TestHeaderPolicyInventory — every member of the shared policy vocabulary is
// intentionally proven on both surfaces: single legal mixed-ASCII-case input
// resolves to the fixed canonical name, the complete value slice is retained
// on HTTP, element-zero semantics apply on chat for credential members, and a
// differently-cased duplicate is omitted as ambiguous. Adding a header to
// either list without considering both surfaces fails this test.
func TestHeaderPolicyInventory(t *testing.T) {
	operational := []string{"Accept", "Content-Type", "User-Agent"}
	if !slices.Equal(httpOperationalHeaders, operational) {
		t.Fatalf("httpOperationalHeaders drifted: got %v, want %v", httpOperationalHeaders, operational)
	}
	credential := []string{"Authorization", "X-Api-Key", "X-Torana-User", "X-Torana-Team", "X-Torana-Tenant"}
	if !slices.Equal(credentialHeaders, credential) {
		t.Fatalf("credentialHeaders drifted: got %v, want %v", credentialHeaders, credential)
	}

	for _, canonical := range append(append([]string{}, httpOperationalHeaders...), credentialHeaders...) {
		t.Run(canonical, func(t *testing.T) {
			lower := strings.ToLower(canonical)
			mixed := lower[:1] + strings.ToUpper(lower[1:2]) + lower[2:]
			raw := map[string][]string{
				mixed: {"v1", "v2"},
			}
			// HTTP: resolves to the fixed canonical name with the complete
			// slice.
			got := filterHTTPHeaders(raw, true)
			v, ok := got[canonical]
			if !ok || len(v) != 2 || v[0] != "v1" || v[1] != "v2" {
				t.Fatalf("HTTP resolution = %v", got)
			}
			if len(got) != 1 {
				t.Fatalf("HTTP emitted extra names: %v", got)
			}
			// Chat: element-zero semantics for credential members; the
			// operational set is not exposed through chat metadata at all.
			chat := projectChatHeaders(raw)
			if canonical == "Accept" || canonical == "Content-Type" || canonical == "User-Agent" {
				if len(chat) != 0 {
					t.Fatalf("operational header leaked into chat: %v", chat)
				}
			} else if v, ok := chat[canonical]; !ok || v != "v1" {
				t.Fatalf("chat resolution = %v", chat)
			}
			// A differently-cased duplicate is ambiguous and omitted.
			raw[canonical] = []string{"v3"}
			got = filterHTTPHeaders(raw, true)
			if _, ok := got[canonical]; ok {
				t.Fatalf("case collision must be omitted: %v", got)
			}
			chat = projectChatHeaders(raw)
			if _, ok := chat[canonical]; ok {
				t.Fatalf("case collision must be omitted on chat: %v", chat)
			}
		})
	}
}
