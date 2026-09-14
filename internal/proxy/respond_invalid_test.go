package proxy

import (
	"context"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func TestSyntheticRenderersRejectUnsupportedBlocks(t *testing.T) {
	response := &pb.SyntheticResponse{Message: &pb.ResponseMessage{Blocks: []*pb.ResponseBlock{{}}}}
	chat := &engine.ChatRequest{Model: "m"}
	if _, err := renderSyntheticJSON("anthropic", chat, response, "id"); err == nil || !strings.Contains(err.Error(), "block 0") {
		t.Fatalf("JSON renderer error = %v", err)
	}
	f := format.Lookup("openai")
	chat.Stream = true
	if _, err := renderSyntheticStream(context.Background(), f, chat, response, "id"); err == nil || !strings.Contains(err.Error(), "block 0") {
		t.Fatalf("stream renderer error = %v", err)
	}
}

func TestSyntheticRenderersRejectMissingMessage(t *testing.T) {
	if _, err := renderSyntheticJSON("openai", &engine.ChatRequest{}, &pb.SyntheticResponse{}, "id"); err == nil {
		t.Fatal("missing message accepted")
	}
}
