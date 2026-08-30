package wasm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
)

// Every fixture whose manifest claims ABI v1 must load as a current plugin guest.
// A compile check cannot prove this because the manifest and binary are separate
// artifacts. Load the real module and verify the contract operators approve.
func TestFixturesClaimingV1UseCurrentABI(t *testing.T) {
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
		if m.ABIVersion != "v1" {
			continue // v1 smoke fixtures are excluded on purpose
		}

		wasmBytes, err := os.ReadFile(filepath.Join(dir, "plugin.wasm"))
		if err != nil {
			// Not built locally. In CI (TORANA_E2E=1) that is a failure: a
			// manifest claiming current ABI with no binary to check is exactly the hole
			// this test exists to close.
			if os.Getenv("TORANA_E2E") != "" {
				t.Errorf("%s claims abi_version v1 but has no plugin.wasm — "+
					"run 'make testdata'", e.Name())
			}
			continue
		}

		p, err := r.LoadPlugin(e.Name(), wasmBytes)
		if err != nil {
			// LoadPlugin reads supported_hooks, so an incompatible guest fails here.
			t.Errorf("%s claims abi_version v1 but does not load as one: %v", e.Name(), err)
			continue
		}
		if p.hooks == 0 {
			t.Errorf("%s exports an empty supported_hooks bitmap; it can never be dispatched", e.Name())
		}
		checked++
	}

	if checked == 0 {
		if os.Getenv("TORANA_E2E") != "" {
			t.Fatal("no built ABI-v1 fixtures present — run 'make testdata'")
		}
		t.Skip("no built ABI-v1 fixtures present; run 'make testdata'")
	}
	t.Logf("verified %d ABI-v1 fixtures", checked)
}

// A current ABI fixture must answer a real dispatch, not merely export the right
// names. Export arity is invisible until the call happens, so a build-only check
// cannot establish guest/host compatibility.
func TestFixturesAnswerARealDispatch(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "plugins", "test-inert-a")
	wasmBytes, err := os.ReadFile(filepath.Join(dir, "plugin.wasm"))
	if err != nil {
		if os.Getenv("TORANA_E2E") != "" {
			t.Fatalf("test-inert-a not built — run 'make testdata': %v", err)
		}
		t.Skip("test-inert-a not built; run 'make testdata'")
	}

	ctx := context.Background()
	r := NewRuntime(ctx)
	defer r.Close()

	p, err := r.LoadPlugin("test-inert-a", wasmBytes)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	input, err := encodeInput(&pbv1.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	if err := p.CallRequest(ctx, pbv1.Hook_HOOK_BEFORE_REQUEST, 1, input, &out); err != nil {
		t.Fatalf("dispatch failed — this is what an arity or export mismatch looks like: %v", err)
	}
	// test-inert-a passes through, so zero bytes is the correct answer.
	if len(out) != 0 {
		t.Fatalf("an inert fixture returned %d bytes, want pass-through", len(out))
	}
}

// encodeInput wraps a request in the current ABI envelope, the way the pipeline does.
func encodeInput(req *pbv1.ChatRequest) ([]byte, error) {
	return proto.Marshal(&pbv1.HookInput{
		RequestId: 1,
		Payload:   &pbv1.HookInput_ChatRequest{ChatRequest: req},
	})
}
