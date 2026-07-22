package wasm

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/sdk/pb"
	"google.golang.org/protobuf/proto"
)

// TestIntentRawABI drives the intent plugin through the raw
// CallRequest ABI (alloc → write → hook → read result) with a real protobuf
// payload, asserting the "i" intent field lands in the returned tool schema.
func TestIntentRawABI(t *testing.T) {
	path := "../../plugins/intent/plugin.wasm"
	requireWASM(t, path)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	r := NewRuntime(context.Background())
	defer r.Close()
	p, err := r.LoadPlugin("intent", b)
	if err != nil {
		t.Fatal(err)
	}
	p.SetGrants([]string{"env.meta_get", "env.meta_set", "env.cache_get", "env.cache_set"})

	req := &pb.ChatRequest{
		Messages: []*pb.Message{{Role: "user", Content: "hi"}},
		Tools: []*pb.ToolDef{{
			Name:           "read",
			ParametersJson: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
	}
	input, _ := proto.Marshal(req)

	var outBytes []byte
	if err := p.CallRequest(context.Background(), "run_before_request", 1, input, &outBytes); err != nil {
		t.Fatal(err)
	}
	if len(outBytes) == 0 {
		t.Fatal("intent plugin did not modify the request (expected intent injection)")
	}

	var out pb.ChatRequest
	if err := proto.Unmarshal(outBytes, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(out.Tools))
	}
	params := string(out.Tools[0].ParametersJson)
	if !strings.Contains(params, `"i"`) {
		t.Fatalf(`expected "i" injected into schema, got %s`, params)
	}
}
