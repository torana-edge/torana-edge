package credentialstore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/torana-edge/torana-edge/internal/secret"
)

func TestStorePersistsEncryptedOwnerOnlyValues(t *testing.T) {
	dir := t.TempDir()
	secrets, err := secret.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "credentials.json")
	store, err := Open(path, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("provider", "SECRET-value"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("SECRET-value")) {
		t.Fatal("credential store contains plaintext")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	reopened, err := Open(path, secrets)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Resolve(context.Background(), "provider")
	if err != nil || string(got) != "SECRET-value" {
		t.Fatalf("Resolve = %q, %v", got, err)
	}
	if err := reopened.Delete("provider"); err != nil {
		t.Fatal(err)
	}
	if len(reopened.List()) != 0 {
		t.Fatal("deleted credential remains listed")
	}
}
