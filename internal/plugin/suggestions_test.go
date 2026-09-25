package plugin

import (
	"bytes"
	"testing"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func TestSuggestionProjectionRestoresMetadataByteExactly(t *testing.T) {
	original := []byte(`{"_provider":"default", "unrelated":1.00}`)
	req := &pb.ChatRequest{ToranaMetaJson: append([]byte(nil), original...)}
	injectSuggestionOutcomes(req, []byte(`[{"id":"sg_one","status":"accepted"}]`))
	if !bytes.Contains(req.ToranaMetaJson, []byte(`"_suggestions"`)) {
		t.Fatalf("missing plugin-scoped outcomes: %s", req.ToranaMetaJson)
	}
	restoreRequestHeaders(req, original)
	if !bytes.Equal(req.ToranaMetaJson, original) {
		t.Fatalf("host metadata changed after projection: %q", req.ToranaMetaJson)
	}
}
