package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// FilePlan contains a reviewed snapshot. File contents remain private so
// previews cannot accidentally print unrelated credentials from the file.
type FilePlan struct {
	Path                  string
	Server                Server
	Changed               bool
	Teardown              bool
	before, ownership     []byte
	existed, ownedExisted bool
	edit                  Edit
	claude                bool
}

func PlanFile(path, harness string, server Server, teardown bool) (*FilePlan, error) {
	if harness != "claude-code" && harness != "codex" {
		return nil, fmt.Errorf("use the generic MCP instructions for %q", harness)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	before, exists, err := readConfig(abs)
	if err != nil {
		return nil, err
	}
	plan := &FilePlan{Path: abs, Server: server, Teardown: teardown, before: before, existed: exists, claude: harness == "claude-code"}
	plan.ownership, plan.ownedExisted, err = readConfig(abs + ".torana-managed.json")
	if err != nil {
		return nil, err
	}
	if plan.claude {
		plan.edit, err = EditClaude(before, plan.ownership, server, teardown)
	} else {
		if teardown && bytes.Contains(before, []byte(begin)) {
			var installed Server
			if !plan.ownedExisted || json.Unmarshal(plan.ownership, &installed) != nil || installed.Command == "" {
				return nil, fmt.Errorf("Torana ownership record is missing or invalid; review the Codex entry manually")
			}
			server = installed
			plan.Server = installed
		}
		plan.edit, err = EditCodex(before, server, teardown)
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
		owned, exists, err := readConfig(p.Path + ".torana-managed.json")
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
	if p.existed {
		file, err := os.CreateTemp(filepath.Dir(p.Path), ".torana-backup-*")
		if err != nil {
			return "", err
		}
		backup = file.Name()
		if err := writeAndClose(file, p.before); err != nil {
			return backup, err
		}
	}
	if !p.Teardown {
		if err := atomicConfig(p.Path+".torana-managed.json", p.edit.Ownership); err != nil {
			return backup, err
		}
	}
	if err := atomicConfig(p.Path, p.edit.Content); err != nil {
		return backup, err
	}
	if p.Teardown {
		// Keep an empty, harmless ownership record instead of deleting files.
		if err := atomicConfig(p.Path+".torana-managed.json", []byte("{}\n")); err != nil {
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
	file, err := os.CreateTemp(filepath.Dir(path), ".torana-mcp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if err := writeAndClose(file, data); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
