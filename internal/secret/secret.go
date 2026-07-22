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
	"strings"
)

const tokenPrefix = "enc:"

// Store holds a machine-local AES-256 key loaded from disk.
type Store struct {
	key []byte
}

// Open loads the key file at <dataDir>/secret.key, creating it (0600) with a
// fresh 32-byte random key on first use. The dataDir is created (0755) if absent.
func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("data directory path cannot be empty")
	}

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	keyPath := filepath.Join(dataDir, "secret.key")
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("failed to generate random key: %w", err)
		}
		if err := os.WriteFile(keyPath, key, 0600); err != nil {
			return nil, fmt.Errorf("failed to write secret key file: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("failed to read secret key file: %w", err)
	} else if len(key) != 32 {
		return nil, fmt.Errorf("invalid key length in secret key file: expected 32 bytes, got %d", len(key))
	}

	keyCopy := make([]byte, len(key))
	copy(keyCopy, key)

	return &Store{key: keyCopy}, nil
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
	if len(data) < nonceSize {
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
