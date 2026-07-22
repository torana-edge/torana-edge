package secret_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/secret"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := secret.Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	testCases := []struct {
		name      string
		plaintext string
	}{
		{"empty string", ""},
		{"simple string", "my-api-key-12345"},
		{"long and unicode string", "🔑 secret-pässword-🔑-with-long-text-1234567890-!@#$%^&*()_+~`-={}|[]\\:\";'<>?,./🚀-café-ñ"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			token, err := store.Encrypt(tc.plaintext)
			if err != nil {
				t.Fatalf("Encrypt failed: %v", err)
			}

			if !secret.IsEncrypted(token) {
				t.Fatalf("IsEncrypted returned false for token %q", token)
			}

			decrypted, err := store.Decrypt(token)
			if err != nil {
				t.Fatalf("Decrypt failed: %v", err)
			}

			if decrypted != tc.plaintext {
				t.Fatalf("Decrypt got %q, want %q", decrypted, tc.plaintext)
			}
		})
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()

	store1, err := secret.Open(dir)
	if err != nil {
		t.Fatalf("Open(1) failed: %v", err)
	}

	plaintext := "persistent-secret-key"
	token, err := store1.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	store2, err := secret.Open(dir)
	if err != nil {
		t.Fatalf("Open(2) failed: %v", err)
	}

	decrypted, err := store2.Decrypt(token)
	if err != nil {
		t.Fatalf("Decrypt with store2 failed: %v", err)
	}

	if decrypted != plaintext {
		t.Fatalf("Decrypt got %q, want %q", decrypted, plaintext)
	}
}

func TestKeyFileMode(t *testing.T) {
	dir := t.TempDir()
	_, err := secret.Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	keyPath := filepath.Join(dir, "secret.key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("Stat secret.key failed: %v", err)
	}

	mode := info.Mode().Perm()
	if mode != 0600 {
		t.Fatalf("secret.key mode got %o, want 0600", mode)
	}
}

func TestTampering(t *testing.T) {
	dir := t.TempDir()
	store, err := secret.Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	token, err := store.Encrypt("super-secret-data")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	raw := strings.TrimPrefix(token, "enc:")
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("DecodeString failed: %v", err)
	}

	// Flip a byte in payload
	decoded[len(decoded)-1] ^= 0xff
	tamperedToken := "enc:" + base64.StdEncoding.EncodeToString(decoded)

	_, err = store.Decrypt(tamperedToken)
	if err == nil {
		t.Fatalf("Decrypt expected error for tampered token, got nil")
	}
}

func TestWrongKey(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	store1, err := secret.Open(dir1)
	if err != nil {
		t.Fatalf("Open(dir1) failed: %v", err)
	}

	store2, err := secret.Open(dir2)
	if err != nil {
		t.Fatalf("Open(dir2) failed: %v", err)
	}

	token, err := store1.Encrypt("secret-for-store1")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	_, err = store2.Decrypt(token)
	if err == nil {
		t.Fatalf("Decrypt with wrong key expected error, got nil")
	}
}

func TestIsEncrypted(t *testing.T) {
	dir := t.TempDir()
	store, err := secret.Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	token, err := store.Encrypt("hello")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	if !secret.IsEncrypted(token) {
		t.Errorf("IsEncrypted(%q) got false, want true", token)
	}

	for _, v := range []string{"", "plain", "env:FOO", "ENC:123", "enc"} {
		if secret.IsEncrypted(v) {
			t.Errorf("IsEncrypted(%q) got true, want false", v)
		}
	}
}

func TestInvalidKeyLength(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "secret.key")
	if err := os.WriteFile(keyPath, []byte("short-key"), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	_, err := secret.Open(dir)
	if err == nil {
		t.Fatalf("Open expected error for invalid key length, got nil")
	}
}
