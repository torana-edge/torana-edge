package wasm
import (
	"context"
	"os"
	"testing"

	"github.com/torana-edge/torana-edge/pkg/pb"
	"google.golang.org/protobuf/proto"
)

func TestDelegator(t *testing.T) {
	b, _ := os.ReadFile("../../plugins/delegator/plugin.wasm")
	r := NewRuntime(context.Background()); defer r.Close()
	p, _ := r.LoadPlugin("delegator", b)
	
	req := &pb.ChatRequest{Model: ""}
	input, _ := proto.Marshal(req)

	var outBytes []byte
	if err := p.CallRequest(context.Background(), "on_chat_request", input, &outBytes); err != nil {
		t.Fatal(err)
	}
	
	var out pb.ChatRequest
	if err := proto.Unmarshal(outBytes, &out); err != nil {
		t.Fatal(err)
	}

	if out.Model != "claude-3-5-sonnet-20241022" {
		t.Fatalf("expected model injection, got %v", out.Model)
	}
	t.Log("delegator OK")
}
