package mcpauth

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/torana-edge/torana-edge/internal/pluginstate"
	"github.com/torana-edge/torana-edge/internal/secret"
)

func TestTokenStorageRestartRotationAndEncryption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	state, err := pluginstate.New(pluginstate.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := secret.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(state, sealer)
	if token, err := manager.Current(); err != nil || token != "" {
		t.Fatal("read created a token")
	}
	token, err := manager.Ensure()
	if err != nil || len(token) != 43 {
		t.Fatal("token creation failed")
	}
	stored, _, found, err := state.GetVersioned(namespace, tokenKey)
	if err != nil || !found || !strings.HasPrefix(stored, "enc:") || strings.Contains(stored, token) {
		t.Fatal("token was not encrypted")
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = pluginstate.New(pluginstate.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	sealer, err = secret.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	manager = New(state, sealer)
	if current, err := manager.Current(); err != nil || current != token {
		t.Fatal("token did not survive restart")
	}
	rotated, err := manager.Rotate()
	if err != nil || rotated == token {
		t.Fatal("rotation failed")
	}
	if current, err := manager.Current(); err != nil || current != rotated {
		t.Fatal("rotation was not committed")
	}
}

func TestConcurrentTokenSetupConverges(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	sealer, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := New(state, sealer)
	var wg sync.WaitGroup
	results := make(chan string, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := manager.Ensure()
			if err != nil {
				t.Error(err)
				return
			}
			results <- token
		}()
	}
	wg.Wait()
	close(results)
	current, err := manager.Current()
	if err != nil {
		t.Fatal(err)
	}
	for token := range results {
		if token != current {
			t.Fatal("setup returned an orphaned token")
		}
	}
}

func TestTokenStorageFailureDoesNotReturnCredential(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{MaxValueBytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	sealer, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := New(state, sealer)
	if token, err := manager.Ensure(); err == nil || token != "" {
		t.Fatal("failed write returned an uncommitted credential")
	}
	if token, err := manager.Current(); err != nil || token != "" {
		t.Fatal("failed write changed current token")
	}
}
