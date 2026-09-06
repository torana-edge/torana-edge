package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/torana-edge/torana-edge/internal/fileperm"
	"strings"
)

const tokenPrefix = "enc:"

// Store holds a machine-local AES-256 key loaded from disk.
type Store struct {
	key []byte
}

// Open loads the key file at <dataDir>/secret.key, creating it (0600) with a
// fresh 32-byte random key on first use. The dataDir is kept private because
// even directory listings expose which local secrets exist.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("data directory path cannot be empty")
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}
	if err := fileperm.RestrictDir(dataDir); err != nil {
		return nil, fmt.Errorf("securing data directory: %w", err)
	}

	keyPath := filepath.Join(dataDir, "secret.key")
	key, err := readExistingKey(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		// O_EXCL, not WriteFile. The server and the `torana credential` CLI
		// both call Open, so a first run that starts both at once had each
		// generate a key and the loser's write clobber the winner's — after
		// which anything already encrypted under the overwritten key can never
		// be decrypted. Losing the race now means reading the key that won.
		key, err = createOrReadKey(keyPath)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	keyCopy := make([]byte, len(key))
	copy(keyCopy, key)

	return &Store{key: keyCopy}, nil
}

// readExistingKey reads the key through ONE handle: opened, restricted,
// checked on that handle, and only then read.
//
// A pre-existing key file is repaired and then CHECKED. The repair alone used
// to be `_ = os.Chmod(...)` with its error discarded, and on Windows that call
// cannot express the invariant at all, so a key file readable by anyone stayed
// readable by anyone and Torana said nothing. This key decrypts every stored
// provider credential.
//
// The check is on the descriptor rather than the path because a name can be
// made to resolve elsewhere between the two: checking the PATH and reading it
// separately would let a safe replacement be verified while the original is
// the one actually read.
func readExistingKey(keyPath string) ([]byte, error) {
	f, err := os.Open(keyPath)
	if err != nil {
		return nil, err // includes os.ErrNotExist, which the caller acts on
	}
	defer func() { _ = f.Close() }()

	if err := fileperm.Restrict(keyPath); err != nil {
		return nil, fmt.Errorf("securing secret key file: %w", err)
	}
	if err := fileperm.VerifyFile(f); err != nil {
		return nil, fmt.Errorf("secret key file: %w", err)
	}
	key, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("failed to read secret key file: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("invalid key length in secret key file: expected 32 bytes, got %d", len(key))
	}
	return key, nil
}

// createOrReadKey writes a fresh 32-byte key exclusively, or reads the one that
// another process created first.
func createOrReadKey(keyPath string) ([]byte, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("failed to generate random key: %w", err)
	}
	// WriteNew is the exclusive create: it fails rather than truncating, and
	// it restricts the file to its owner BEFORE the key bytes go in, so the
	// key is never briefly readable through an inherited ACL.
	err := fileperm.WriteNew(keyPath, key)
	if errors.Is(err, os.ErrExist) {
		// The loser of the race reads the winner's key through the same
		// handle-bound path as any other existing key.
		return readExistingKey(keyPath)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create secret key file: %w", err)
	}
	return key, nil
}

// Encrypt returns a self-describing token "enc:" + base64(nonce||ciphertext).
func (s *Store) Encrypt(plaintext string) (string, error) {
	if s == nil || len(s.key) == 0 {
		return "", fmt.Errorf("store is not initialized")
	}

	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return tokenPrefix + base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt reverses Encrypt. Returns an error if the token is not a valid
// "enc:"-prefixed value or authentication fails.
func (s *Store) Decrypt(token string) (string, error) {
	if s == nil || len(s.key) == 0 {
		return "", fmt.Errorf("store is not initialized")
	}

	if !IsEncrypted(token) {
		return "", fmt.Errorf("invalid secret token format: missing enc: prefix")
	}

	raw := strings.TrimPrefix(token, tokenPrefix)
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("invalid base64 encoding in token: %w", err)
	}

	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize+gcm.Overhead() {
		return "", fmt.Errorf("invalid token length: payload too short")
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintextBytes, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt token: %w", err)
	}

	return string(plaintextBytes), nil
}

// IsEncrypted reports whether v is an "enc:"-prefixed token produced by Encrypt.
func IsEncrypted(v string) bool {
	return strings.HasPrefix(v, tokenPrefix)
}
