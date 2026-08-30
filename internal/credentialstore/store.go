package credentialstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/torana-edge/torana-edge/internal/secret"
)

// Store is Torana's built-in machine-local credential provider. Values are
// encrypted individually and the index is written atomically with owner-only
// permissions. It implements credential.Provider.
type Store struct {
	mu      sync.RWMutex
	path    string
	secrets *secret.Store
	values  map[string]string
}

func Open(path string, secrets *secret.Store) (*Store, error) {
	if path == "" || secrets == nil {
		return nil, fmt.Errorf("credential store path and encryption store are required")
	}
	s := &Store{path: path, secrets: secrets, values: map[string]string{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read credential store: %w", err)
	}
	if err := json.Unmarshal(raw, &s.values); err != nil {
		return nil, fmt.Errorf("decode credential store: %w", err)
	}
	return s, nil
}

func (s *Store) Resolve(_ context.Context, key string) ([]byte, error) {
	s.mu.RLock()
	encrypted, ok := s.values[key]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("local credential %q is not set", key)
	}
	value, err := s.secrets.Decrypt(encrypted)
	if err != nil {
		return nil, fmt.Errorf("decrypt local credential %q: %w", key, err)
	}
	return []byte(value), nil
}

func (s *Store) Set(key, value string) error {
	if key == "" || value == "" {
		return fmt.Errorf("credential key and value are required")
	}
	encrypted, err := s.secrets.Encrypt(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneValues(s.values)
	next[key] = encrypted
	if err := s.persistLocked(next); err != nil {
		return err
	}
	s.values = next
	return nil
}

func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneValues(s.values)
	delete(next, key)
	if err := s.persistLocked(next); err != nil {
		return err
	}
	s.values = next
	return nil
}

func (s *Store) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.values))
	for key := range s.values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func cloneValues(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (s *Store) persistLocked(values map[string]string) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		return err
	}
	return os.Chmod(s.path, 0o600)
}
