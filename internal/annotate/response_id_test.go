package annotate

import (
	"testing"

	"github.com/torana-edge/torana-edge/internal/secret"
)

func TestLocalResponseIDSurvivesRestartAndRejectsTampering(t *testing.T) {
	directory := t.TempDir()
	first, err := secret.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	id, err := EncodeLocalResponseID(first, "resp_real_123")
	if err != nil {
		t.Fatal(err)
	}
	second, err := secret.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	got, recognized, err := DecodeLocalResponseID(second, id)
	if err != nil || !recognized || got != "resp_real_123" {
		t.Fatalf("decoded %q, recognized %v, err %v", got, recognized, err)
	}
	if got, recognized, err := DecodeLocalResponseID(second, "resp_provider"); err != nil || recognized || got != "resp_provider" {
		t.Fatalf("ordinary provider ID altered: %q, %v, %v", got, recognized, err)
	}
	// The provider ID is encoded, so changing plaintext would be a no-op.
	// Always change a MAC nibble, including when it already ends in zero.
	replacement := "0"
	if id[len(id)-1] == '0' {
		replacement = "1"
	}
	bad := id[:len(id)-1] + replacement
	if _, recognized, err := DecodeLocalResponseID(second, bad); !recognized || err == nil {
		t.Fatalf("tampered local ID accepted: recognized %v, err %v", recognized, err)
	}
}
