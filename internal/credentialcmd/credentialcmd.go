package credentialcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/term"

	"github.com/torana-edge/torana-edge/internal/credentialstore"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/secret"
)

func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 || args[0] != "credential" {
		Usage(stderr)
		return errors.New("credential subcommand required")
	}
	switch args[1] {
	case "set":
		return set(args[2:], stdout, stderr)
	case "list", "ls":
		return list(args[2:], stdout)
	case "delete", "rm":
		return remove(args[2:], stdout)
	case "help", "-h", "--help":
		Usage(stdout)
		return nil
	default:
		Usage(stderr)
		return fmt.Errorf("unknown credential command %q", args[1])
	}
}

func Usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Usage:")
	_, _ = fmt.Fprintln(w, "  torana credential set <id>             securely read and store a local value")
	_, _ = fmt.Fprintln(w, "  torana credential set <id> --env NAME  resolve the value from NAME at runtime")
	_, _ = fmt.Fprintln(w, "  torana credential list")
	_, _ = fmt.Fprintln(w, "  torana credential delete <id>")
}

func load() (string, provider.Config, *credentialstore.Store, error) {
	path, err := provider.ManagedStorePath()
	if err != nil {
		return "", provider.Config{}, nil, err
	}
	cfg, err := provider.Load(path)
	if err != nil {
		return "", provider.Config{}, nil, err
	}
	sec, err := secret.Open(filepath.Dir(path))
	if err != nil {
		return "", provider.Config{}, nil, err
	}
	store, err := credentialstore.Open(filepath.Join(filepath.Dir(path), "credentials.json"), sec)
	return path, cfg, store, err
}

func set(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "" {
		return errors.New("credential id is required")
	}
	id := args[0]
	path, cfg, store, err := load()
	if err != nil {
		return err
	}
	if cfg.Credentials.Sources == nil {
		cfg.Credentials.Sources = map[string]provider.CredentialSource{}
	}
	if cfg.Credentials.Entries == nil {
		cfg.Credentials.Entries = map[string]provider.CredentialEntry{}
	}
	previous, hadPrevious := cfg.Credentials.Entries[id]
	if len(args) == 3 && args[1] == "--env" {
		if args[2] == "" {
			return errors.New("--env requires a variable name")
		}
		cfg.Credentials.Sources["env"] = provider.CredentialSource{Type: "env"}
		cfg.Credentials.Entries[id] = provider.CredentialEntry{Source: "env", Key: args[2]}
		if err := provider.Save(path, cfg); err != nil {
			return err
		}
	} else if len(args) == 1 {
		value, err := readSecret(stderr)
		if err != nil {
			return err
		}
		oldValue, oldErr := store.Resolve(context.Background(), id)
		hadOldValue := oldErr == nil
		if err := store.Set(id, value); err != nil {
			return err
		}
		cfg.Credentials.Sources["local"] = provider.CredentialSource{Type: "local"}
		cfg.Credentials.Entries[id] = provider.CredentialEntry{Source: "local", Key: id}
		if err := provider.Save(path, cfg); err != nil {
			var rollbackErr error
			if hadOldValue {
				rollbackErr = store.Set(id, string(oldValue))
			} else {
				rollbackErr = store.Delete(id)
			}
			if rollbackErr != nil {
				return fmt.Errorf("save credential config: %v (rollback failed: %w)", err, rollbackErr)
			}
			return err
		}
	} else {
		return errors.New("usage: torana credential set <id> [--env NAME]")
	}
	current := cfg.Credentials.Entries[id]
	if hadPrevious && previous.Source == "local" && previous.Key != "" && previous != current && !credentialKeyInUse(cfg, id, previous) {
		if err := store.Delete(previous.Key); err != nil {
			return fmt.Errorf("credential was configured, but obsolete local value could not be removed: %w", err)
		}
	}
	fmt.Fprintf(stdout, "Configured credential %s\n", id)
	return nil
}

func readSecret(stderr io.Writer) (string, error) {
	var raw []byte
	var err error
	if term.IsTerminal(int(os.Stdin.Fd())) {
		_, _ = fmt.Fprint(stderr, "Credential value: ")
		raw, err = term.ReadPassword(int(os.Stdin.Fd()))
		_, _ = fmt.Fprintln(stderr)
	} else {
		raw, err = io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
	}
	if err != nil {
		return "", err
	}
	value := normalizeSecretInput(raw)
	if value == "" {
		return "", errors.New("credential value must not be empty")
	}
	return value, nil
}

// normalizeSecretInput removes only terminal line delimiters introduced by
// shell input. Spaces and tabs can be meaningful credential bytes and must not
// be silently rewritten.
func normalizeSecretInput(raw []byte) string {
	return string(bytes.TrimRight(raw, "\r\n"))
}

func list(args []string, stdout io.Writer) error {
	if len(args) != 0 {
		return errors.New("credential list takes no arguments")
	}
	_, cfg, _, err := load()
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(cfg.Credentials.Entries))
	for id := range cfg.Credentials.Entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Fprintf(stdout, "%s\t%s\n", id, cfg.Credentials.Entries[id].Source)
	}
	return nil
}

func remove(args []string, stdout io.Writer) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return errors.New("credential id is required")
	}
	id := args[0]
	path, cfg, store, err := load()
	if err != nil {
		return err
	}
	entry, existed := cfg.Credentials.Entries[id]
	delete(cfg.Credentials.Entries, id)
	if err := provider.Save(path, cfg); err != nil {
		return err
	}
	if existed && entry.Source == "local" && !credentialKeyInUse(cfg, id, entry) {
		if err := store.Delete(entry.Key); err != nil {
			return fmt.Errorf("credential was removed, but its obsolete local value could not be deleted: %w", err)
		}
	}
	fmt.Fprintf(stdout, "Deleted credential %s\n", id)
	return nil
}

func credentialKeyInUse(cfg provider.Config, excludedID string, candidate provider.CredentialEntry) bool {
	for id, entry := range cfg.Credentials.Entries {
		if id != excludedID && entry.Source == candidate.Source && entry.Key == candidate.Key {
			return true
		}
	}
	return false
}
