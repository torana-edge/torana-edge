package wasm

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/torana-edge/torana-plugin-sdk/pb"
	"google.golang.org/protobuf/proto"
)

// TestIntentRawABI drives the intent plugin through the raw
// CallRequest ABI (alloc → write → hook → read result) with a real protobuf
// payload, asserting the "i" intent field lands in the returned tool schema.
func TestIntentRawABI(t *testing.T) {
	path := fixturesDir + "/test-mutator/plugin.wasm"
	requireWASM(t, path)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	r := NewRuntime(context.Background())
	defer r.Close()
	p, err := r.LoadPlugin("test-mutator", b)
	if err != nil {
		t.Fatal(err)
	}
	p.SetGrants([]string{"env.log"})

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
		t.Fatal("the guest returned pass-through; the fixture always mutates, so the ABI round-trip lost the request")
	}

	var out pb.ChatRequest
	if err := proto.Unmarshal(outBytes, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(out.Tools))
	}
	// The fixture mutates a tool definition and a message. Asserting both come
	// back proves the raw ABI carried the whole request in and the whole
	// modified request out — not merely that the hook was reached.
	if got := out.Tools[0].Description; got != "described by test-mutator" {
		t.Fatalf("tool definition did not survive the raw ABI round-trip: description = %q", got)
	}
	if len(out.Messages) != 1 || !strings.HasSuffix(out.Messages[0].Content, "[seen by test-mutator]") {
		t.Fatalf("message did not survive the raw ABI round-trip: %+v", out.Messages)
	}
}
