package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/torana-edge/torana-edge/internal/provider"
	"os"
	"path/filepath"
)

// FilePlan contains a reviewed snapshot. File contents remain private so
// previews cannot accidentally print unrelated credentials from the file.
type FilePlan struct {
	Path                       string
	Server                     Server
	Changed                    bool
	Teardown                   bool
	before, ownership          []byte
	existed, ownedExisted      bool
	edit                       Edit
	claude                     bool
	ownershipPath, recoveryDir string
	mode                       os.FileMode
}

func PlanFile(path, harness string, server Server, teardown bool) (*FilePlan, error) {
	if harness != "claude-code" && harness != "codex" {
		return nil, fmt.Errorf("use the generic MCP instructions for %q", harness)
	}
	plan, err := planSnapshot(path)
	if err != nil {
		return nil, err
	}
	plan.Server, plan.Teardown, plan.claude = server, teardown, harness == "claude-code"
	if plan.claude {
		plan.edit, err = EditClaude(plan.before, plan.ownership, server, teardown)
	} else {
		if teardown && bytes.Contains(plan.before, []byte(begin)) {
			var installed Server
			if !plan.ownedExisted || json.Unmarshal(plan.ownership, &installed) != nil || installed.Command == "" {
				return nil, fmt.Errorf("Torana ownership record is missing or invalid; review the Codex entry manually")
			}
			server = installed
			plan.Server = installed
		}
		plan.edit, err = EditCodex(plan.before, server, teardown)
		if err == nil && plan.edit.Changed && !teardown {
			plan.edit.Ownership, err = json.Marshal(server)
		}
	}
	if err != nil {
		return nil, err
	}
	plan.Changed = plan.edit.Changed
	return plan, nil
}

func planSnapshot(path string) (*FilePlan, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	before, exists, err := readConfig(abs)
	if err != nil {
		return nil, err
	}
	plan := &FilePlan{Path: abs, before: before, existed: exists}
	store, err := provider.ManagedStorePath()
	if err != nil {
		return nil, err
	}
	plan.recoveryDir = filepath.Join(filepath.Dir(store), "harness")
	hash := sha256.Sum256([]byte(abs))
	plan.ownershipPath = filepath.Join(plan.recoveryDir, fmt.Sprintf("%x.json", hash))
	plan.mode = 0o600
	if exists {
		info, statErr := os.Stat(abs)
		if statErr != nil {
			return nil, statErr
		}
		plan.mode = info.Mode().Perm()
	}
	plan.ownership, plan.ownedExisted, err = readConfig(plan.ownershipPath)
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func readConfig(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("refusing to edit a symlink or non-regular configuration file: %s", path)
	}
	if info.Size() > 1<<20 {
		return nil, false, fmt.Errorf("configuration exceeds the 1 MiB safe edit limit: %s", path)
	}
	data, err := os.ReadFile(path)
	return data, true, err
}

// Apply rechecks the preview, writes a private recovery backup, and replaces
// the config atomically. It never restores a whole backup during teardown.
// Ownership is written before adding the entry and cleared only
// after removal: interruption cannot leave a newly installed entry unowned.
func (p *FilePlan) Apply() (backup string, err error) {
	if !p.Changed {
		return "", nil
	}
	current, exists, err := readConfig(p.Path)
	if err != nil {
		return "", err
	}
	if exists != p.existed || !bytes.Equal(current, p.before) {
		return "", fmt.Errorf("configuration changed since preview; review a fresh plan")
	}
	{
		owned, exists, err := readConfig(p.ownershipPath)
		if err != nil {
			return "", err
		}
		if exists != p.ownedExisted || !bytes.Equal(owned, p.ownership) {
			return "", fmt.Errorf("ownership changed since preview; review a fresh plan")
		}
	}
	if err := os.MkdirAll(filepath.Dir(p.Path), 0o700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(p.recoveryDir, 0o700); err != nil {
		return "", err
	}
	if p.existed {
		file, err := os.CreateTemp(p.recoveryDir, "backup-*")
		if err != nil {
			return "", err
		}
		backup = file.Name()
		if err := writeAndClose(file, p.before); err != nil {
			return backup, err
		}
	}
	if !p.Teardown {
		if err := atomicConfig(p.ownershipPath, p.edit.Ownership); err != nil {
			return backup, err
		}
	}
	if err := atomicConfigMode(p.Path, p.edit.Content, p.mode); err != nil {
		return backup, err
	}
	if p.Teardown {
		// Keep an empty, harmless ownership record instead of deleting files.
		if err := atomicConfig(p.ownershipPath, []byte("{}\n")); err != nil {
			return backup, fmt.Errorf("configuration was removed, but ownership cleanup failed: %w", err)
		}
	}
	return backup, nil
}

func writeAndClose(file *os.File, data []byte) error {
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func atomicConfig(path string, data []byte) error {
	return atomicConfigMode(path, data, 0o600)
}

func atomicConfigMode(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".torana-mcp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if err := preserveGroup(file, path); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if err := writeAndClose(file, data); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
