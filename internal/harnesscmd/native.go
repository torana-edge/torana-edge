package harnesscmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/torana-edge/torana-edge/internal/harness"
	"github.com/torana-edge/torana-edge/internal/provider"
)

type commandRunner func(string, ...string) ([]byte, error)

func runNative(binary string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	var buffer boundedOutput
	command.Stdout, command.Stderr = &buffer, &buffer
	err := command.Run()
	return buffer.Bytes(), err
}

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(data []byte) (int, error) {
	if b.Len()+len(data) > 1<<20 {
		return 0, fmt.Errorf("harness command output exceeded safe limit")
	}
	return b.Buffer.Write(data)
}

type nativePlan struct {
	binary, name, scope, config, record string
	args                                []string
	server                              harness.Server
	before                              string
	changed, teardown                   bool
	run                                 commandRunner
}

func fingerprint(raw []byte) string {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return ""
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(canonical))
}

// Only the expected stdio transport is eligible for removal. Unexpected
// environment, cwd or transport settings mean the entry belongs to the user.
func sameServer(raw []byte, expected harness.Server) bool {
	var entry struct {
		Type    string            `json:"type"`
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
		EnvVars []string          `json:"env_vars"`
		Cwd     *string           `json:"cwd"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&entry) != nil {
		return false
	}
	return (entry.Type == "" || entry.Type == "stdio") && entry.Command == expected.Command && reflect.DeepEqual(entry.Args, expected.Args) && len(entry.Env) == 0 && len(entry.EnvVars) == 0 && entry.Cwd == nil
}

func (p *nativePlan) inspect() ([]byte, bool, error) {
	args := []string{"mcp", "get", "torana"}
	if p.name == "codex" {
		args = append(args, "--json")
	}
	output, err := p.run(p.binary, args...)
	if err != nil {
		if strings.HasPrefix(strings.TrimSpace(string(output)), "No MCP server named") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("cannot inspect Torana through %s mcp get; inspect it manually (command output kept private)", p.binary)
	}
	if p.name == "codex" {
		var root struct {
			Transport json.RawMessage `json:"transport"`
			Enabled   *bool           `json:"enabled"`
		}
		if json.Unmarshal(output, &root) != nil || len(root.Transport) == 0 || root.Enabled != nil && !*root.Enabled {
			return nil, false, fmt.Errorf("unexpected or disabled Codex MCP entry; review it manually")
		}
		return root.Transport, true, nil
	}
	// Claude's get command is human-readable; never guess ownership by parsing
	// its text. Read only the documented scope entry, without rewriting files.
	file, err := os.Open(p.config)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, false, fmt.Errorf("Claude MCP config cannot be safely inspected")
	}
	var root struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(data, &root) != nil || len(root.Servers["torana"]) == 0 {
		return nil, false, fmt.Errorf("Torana exists in a different or unknown Claude scope; review it manually")
	}
	return root.Servers["torana"], true, nil
}

func planNative(binary, name, scope, config string, server harness.Server, teardown bool, run commandRunner) (*nativePlan, error) {
	store, err := provider.ManagedStorePath()
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(config)
	if err != nil {
		return nil, err
	}
	key := sha256.Sum256([]byte(name + "\x00" + scope + "\x00" + abs))
	p := &nativePlan{binary: binary, name: name, scope: scope, config: config, server: server, teardown: teardown, run: run, record: filepath.Join(filepath.Dir(store), "harness", fmt.Sprintf("native-%x.json", key))}
	entry, exists, err := p.inspect()
	if err != nil {
		return nil, err
	}
	p.before = fingerprint(entry)
	if teardown && exists {
		owned, err := os.ReadFile(p.record)
		var record struct {
			Server      harness.Server `json:"server"`
			Fingerprint string         `json:"fingerprint"`
		}
		if err != nil || json.Unmarshal(owned, &record) != nil || record.Fingerprint != p.before || !sameServer(entry, record.Server) {
			return nil, fmt.Errorf("Torana MCP entry is unowned or edited; review it manually")
		}
		p.server = record.Server
	} else if exists && !sameServer(entry, server) {
		return nil, fmt.Errorf("a different Torana MCP entry exists; keep it or remove it explicitly")
	}
	p.changed = teardown && exists || !teardown && !exists
	if teardown {
		p.args = []string{"mcp", "remove"}
	} else {
		p.args = []string{"mcp", "add"}
	}
	if name == "claude-code" {
		p.args = append(p.args, "--scope", scope)
	}
	p.args = append(p.args, "torana")
	if !teardown {
		p.args = append(p.args, "--", server.Command)
		p.args = append(p.args, server.Args...)
	}
	return p, nil
}

func (p *nativePlan) apply() error {
	entry, _, err := p.inspect()
	if err != nil {
		return err
	}
	if fingerprint(entry) != p.before {
		return fmt.Errorf("harness entry changed since preview; review a fresh plan")
	}
	// Keep ownership before addition: if interrupted, repeating setup can
	// recognize the exact entry. Never remove unrelated harness state.
	if !p.teardown {
		candidate := struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		}{"stdio", p.server.Command, p.server.Args}
		raw, _ := json.Marshal(candidate)
		owned, _ := json.Marshal(struct {
			Server      harness.Server `json:"server"`
			Fingerprint string         `json:"fingerprint"`
		}{p.server, fingerprint(raw)})
		if err := os.MkdirAll(filepath.Dir(p.record), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p.record, owned, 0o600); err != nil {
			return err
		}
	}
	if _, err := p.run(p.binary, p.args...); err != nil {
		return fmt.Errorf("harness command failed; inspect the Torana entry before retrying (output kept private)")
	}
	if !p.teardown {
		entry, exists, err := p.inspect()
		if err != nil || !exists || !sameServer(entry, p.server) {
			return fmt.Errorf("harness changed configuration; inspect the Torana entry before retrying")
		}
		owned, _ := json.Marshal(struct {
			Server      harness.Server `json:"server"`
			Fingerprint string         `json:"fingerprint"`
		}{p.server, fingerprint(entry)})
		return os.WriteFile(p.record, owned, 0o600)
	}
	return os.WriteFile(p.record, []byte("{}\n"), 0o600)
}
