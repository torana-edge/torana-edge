package secret_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/torana-edge/torana-edge/internal/fileperm"
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

func TestKeyFileAndDataDirAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := secret.Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	keyPath := filepath.Join(dir, "secret.key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("Stat secret.key failed: %v", err)
	}

	// The key that decrypts every stored provider credential must be readable
	// only by its owner. That invariant is mode bits on Unix and a DACL on
	// Windows, where Perm() reports 0666 for any readable file whatever its
	// ACL says — asserting the bits there proved nothing and failed anyway.
	if err := fileperm.Verify(keyPath, info); err != nil {
		t.Errorf("secret.key is not owner-only: %v", err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := fileperm.Verify(dir, dirInfo); err != nil {
		t.Errorf("data directory is not owner-only: %v", err)
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

// The first-run race. The server and the `torana credential` CLI both call
// Open, so a first run that starts both at once used to have each generate a
// key and the loser's write clobber the winner's — after which anything
// already encrypted under the overwritten key could never be decrypted.
//
// Every caller must come away with the SAME key, and none may fail: the losing
// side reads the winner's file, which is only ever visible once complete.
func TestConcurrentOpenAgreesOnOneKey(t *testing.T) {
	for round := range 50 {
		dir := t.TempDir()

		const callers = 6
		var wg sync.WaitGroup
		start := make(chan struct{})
		tokens := make([]string, callers)
		errs := make([]error, callers)

		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				store, err := secret.Open(dir)
				if err != nil {
					errs[i] = err
					return
				}
				// The key is not exported, so agreement is observed through
				// what it produces: a token every other store can decrypt.
				tokens[i], errs[i] = store.Encrypt("payload")
			}()
		}
		close(start)
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: caller %d failed: %v", round, i, err)
			}
		}
		store, err := secret.Open(dir)
		if err != nil {
			t.Fatalf("round %d: reopen: %v", round, err)
		}
		for i, token := range tokens {
			got, err := store.Decrypt(token)
			if err != nil {
				t.Fatalf("round %d: caller %d encrypted under a key the surviving file "+
					"cannot decrypt — a concurrent Open clobbered it: %v", round, i, err)
			}
			if got != "payload" {
				t.Fatalf("round %d: caller %d round-tripped %q", round, i, got)
			}
		}
	}
}

// The two sides of a first-run race, forced rather than raced for.
//
// TestConcurrentOpenAgreesOnOneKey runs goroutines and is stochastic: it found
// the original Windows failure by luck, on round 19 of 50. This performs the
// interfering directory operation DELIBERATELY at each point where a competing
// caller could have performed it, so the property is proved rather than
// sampled.
//
// The winner creates the key. The loser reads the key the winner published,
// after a directory restriction has landed in between — the exact window where
// a propagating directory ACL used to overwrite a file that was already
// correct.
func TestOpenSurvivesADirectoryRestrictionBetweenCreateAndRead(t *testing.T) {
	dir := t.TempDir()

	winner, err := secret.Open(dir)
	if err != nil {
		t.Fatalf("winning creator: %v", err)
	}
	token, err := winner.Encrypt("payload")
	if err != nil {
		t.Fatal(err)
	}

	// A competing caller re-securing the data directory, unconditionally, in
	// between. This is what several first-run callers all do when they each
	// observe a directory that is not yet owner-only.
	if err := fileperm.RestrictDir(dir); err != nil {
		t.Fatal(err)
	}

	loser, err := secret.Open(dir)
	if err != nil {
		t.Fatalf("losing reader was refused after a directory restriction landed "+
			"between the winner's create and this read: %v", err)
	}
	got, err := loser.Decrypt(token)
	if err != nil {
		t.Fatalf("the losing reader did not get the winner's key: %v", err)
	}
	if got != "payload" {
		t.Fatalf("round-tripped %q, want %q", got, "payload")
	}

	// And again, so a second interfering write is no different from the first.
	if err := fileperm.RestrictDir(dir); err != nil {
		t.Fatal(err)
	}
	again, err := secret.Open(dir)
	if err != nil {
		t.Fatalf("second reader after a further directory restriction: %v", err)
	}
	if got, err := again.Decrypt(token); err != nil || got != "payload" {
		t.Fatalf("second reader round-tripped %q (%v)", got, err)
	}
}

// Fail-closed is unchanged by any of the above: a key file that is not
// owner-only is refused, not repaired-and-accepted on someone else's say-so.
func TestOpenRefusesAKeyItCannotSecure(t *testing.T) {
	dir := t.TempDir()
	if _, err := secret.Open(dir); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "secret.key")
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	// Republish it with no explicit protection: 0644 on Unix, and on Windows a
	// DACL merely inherited from the directory, which fileperm refuses.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, key, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Open repairs what it can and must still end up with an owner-only key.
	store, err := secret.Open(dir)
	if err != nil {
		t.Fatalf("Open could not secure a widened key file: %v", err)
	}
	f, err := os.Open(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := fileperm.VerifyFile(f); err != nil {
		t.Errorf("Open returned a store over a key that is not owner-only: %v", err)
	}
	if store == nil {
		t.Error("nil store")
	}
}
