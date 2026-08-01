package wasm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
)

// Every fixture whose manifest claims ABI v2 must actually be a v2 guest.
//
// During the migration a bulk edit set `"abi_version": "v2"` on all 19 fixture
// manifests, including two polyglot ones whose Rust and AssemblyScript sources
// still exported the three-argument v1 hook. They were outside TESTDATA_DIRS,
// so nothing built them and nothing noticed — a manifest advertising a contract
// its binary does not implement, which the host would only discover at load or
// first dispatch.
//
// A compile check cannot catch this: the manifest and the binary are separate
// artifacts, and the manifest is what an operator approves. This loads the real
// module and asks it.
func TestFixturesClaimingV2AreActuallyV2(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "plugins")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}

	ctx := context.Background()
	r := NewRuntime(ctx)
	defer r.Close()

	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
		if err != nil {
			continue // not a fixture with a manifest
		}
		var m struct {
			ABIVersion string `json:"abi_version"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Errorf("%s: manifest does not parse: %v", e.Name(), err)
			continue
		}
		if m.ABIVersion != "v2" {
			continue // v1 smoke fixtures are excluded on purpose
		}

		wasmBytes, err := os.ReadFile(filepath.Join(dir, "plugin.wasm"))
		if err != nil {
			// Not built locally. In CI (TORANA_E2E=1) that is a failure: a
			// manifest claiming v2 with no binary to check is exactly the hole
			// this test exists to close.
			if os.Getenv("TORANA_E2E") != "" {
				t.Errorf("%s claims abi_version v2 but has no plugin.wasm — "+
					"run 'make testdata'", e.Name())
			}
			continue
		}

		p, err := r.LoadPlugin(e.Name(), wasmBytes)
		if err != nil {
			// LoadPlugin reads supported_hooks, so a v1 guest fails here with
			// "exports no supported_hooks" — which is the point.
			t.Errorf("%s claims abi_version v2 but does not load as one: %v", e.Name(), err)
			continue
		}
		if p.hooks == 0 {
			t.Errorf("%s exports an empty supported_hooks bitmap; it can never be dispatched", e.Name())
		}
		checked++
	}

	if checked == 0 {
		t.Skip("no built v2 fixtures present; run 'make testdata'")
	}
	t.Logf("verified %d fixtures claiming v2", checked)
}

// A v2 fixture must answer a real dispatch, not merely export the right names.
// Export arity is invisible until the call happens: the host passed three
// arguments to a two-argument run_hook through this entire migration and every
// build stayed green.
func TestV2FixturesAnswerARealDispatch(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "plugins", "test-inert-a")
	wasmBytes, err := os.ReadFile(filepath.Join(dir, "plugin.wasm"))
	if err != nil {
		t.Skip("test-inert-a not built; run 'make testdata'")
	}

	ctx := context.Background()
	r := NewRuntime(ctx)
	defer r.Close()

	p, err := r.LoadPlugin("test-inert-a", wasmBytes)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	input, err := encodeInput(&pbv2.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	if err := p.CallRequest(ctx, pbv2.Hook_HOOK_BEFORE_REQUEST, 1, input, &out); err != nil {
		t.Fatalf("dispatch failed — this is what an arity or export mismatch looks like: %v", err)
	}
	// test-inert-a passes through, so zero bytes is the correct answer.
	if len(out) != 0 {
		t.Fatalf("an inert fixture returned %d bytes, want pass-through", len(out))
	}
}

// encodeInput wraps a request in the v2 envelope, the way the pipeline does.
func encodeInput(req *pbv2.ChatRequest) ([]byte, error) {
	return proto.Marshal(&pbv2.HookInput{
		RequestId: 1,
		Payload:   &pbv2.HookInput_ChatRequest{ChatRequest: req},
	})
}
