// Package harness builds narrowly owned MCP configuration changes. It never
// edits provider, model, approval, workspace-trust or unrelated server settings.
package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/BurntSushi/toml"
)

const begin = "# BEGIN TORANA MANAGED MCP v1"
const end = "# END TORANA MANAGED MCP v1"

type Server struct {
	Type    string   `json:"type,omitempty" toml:"-"`
	Command string   `json:"command" toml:"command"`
	Args    []string `json:"args" toml:"args"`
}

// Edit is private file content, not a printable preview (other entries may
// contain credentials). CLI previews should show only the owned server entry.
type Edit struct {
	Content   []byte
	Ownership []byte
	Changed   bool
}

func entryDigest(entry json.RawMessage) string {
	var parsed any
	if json.Unmarshal(entry, &parsed) != nil {
		return ""
	}
	canonical, _ := json.Marshal(parsed)
	hash := sha256.Sum256(canonical)
	return hex.EncodeToString(hash[:])
}

// EditClaude works for project .mcp.json and the user .claude.json. Ownership
// is kept outside the harness schema; teardown refuses an edited server entry.
func EditClaude(current, ownership []byte, server Server, teardown bool) (Edit, error) {
	if len(current) > 1<<20 {
		return Edit{}, fmt.Errorf("Claude configuration exceeds the 1 MiB safe edit limit")
	}
	root := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(current)) != 0 && !uniqueJSONKeys(current) {
		return Edit{}, fmt.Errorf("Claude configuration has invalid JSON or duplicate keys; resolve it before setup")
	}
	if len(bytes.TrimSpace(current)) != 0 && (json.Unmarshal(current, &root) != nil || root == nil) {
		return Edit{}, fmt.Errorf("Claude MCP configuration must be a JSON object")
	}
	servers := map[string]json.RawMessage{}
	if raw, exists := root["mcpServers"]; exists && (json.Unmarshal(raw, &servers) != nil || servers == nil) {
		return Edit{}, fmt.Errorf("mcpServers must be an object")
	}
	var owned struct {
		Digest string `json:"digest"`
	}
	if len(ownership) != 0 && json.Unmarshal(ownership, &owned) != nil {
		return Edit{}, fmt.Errorf("Torana ownership record is invalid")
	}
	existing, exists := servers["torana"]
	if teardown {
		if !exists {
			return Edit{Content: current}, nil
		}
		if owned.Digest == "" || entryDigest(existing) != owned.Digest {
			return Edit{}, fmt.Errorf("Torana entry is not owned or was edited; remove it manually after review")
		}
		delete(servers, "torana")
	} else {
		server.Type = "stdio"
		candidate, err := json.Marshal(server)
		if err != nil {
			return Edit{}, err
		}
		digest := entryDigest(candidate)
		if exists {
			if entryDigest(existing) == digest {
				return Edit{Content: current, Ownership: ownership}, nil
			}
			return Edit{}, fmt.Errorf("a different torana MCP server already exists; keep it or remove it explicitly")
		}
		servers["torana"] = candidate
		ownership, _ = json.Marshal(struct {
			Digest string `json:"digest"`
		}{digest})
	}
	root["mcpServers"], _ = json.Marshal(servers)
	after, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return Edit{}, err
	}
	after = append(after, '\n')
	if teardown {
		ownership = nil
	}
	return Edit{Content: after, Ownership: ownership, Changed: true}, nil
}

// EditCodex preserves every byte outside the appended owned block. A real
// TOML parser catches quoted/inline tables and duplicate definitions; regex
// matching is not sufficient to safely detect a pre-existing server.
func EditCodex(current []byte, server Server, teardown bool) (Edit, error) {
	if len(current) > 1<<20 {
		return Edit{}, fmt.Errorf("Codex configuration exceeds the 1 MiB safe edit limit")
	}
	text := string(current)
	var parsed map[string]any
	if _, err := toml.Decode(text, &parsed); err != nil {
		return Edit{}, fmt.Errorf("Codex configuration is not valid TOML")
	}
	block, err := codexBlock(server)
	if err != nil {
		return Edit{}, err
	}
	start, finish := strings.Index(text, begin), strings.Index(text, end)
	if start >= 0 || finish >= 0 {
		if start < 0 || finish < start || strings.Count(text, begin) != 1 || strings.Count(text, end) != 1 {
			return Edit{}, fmt.Errorf("Torana MCP ownership markers are incomplete or ambiguous")
		}
		finish += len(end)
		owned := text[start:finish]
		if owned != strings.TrimSuffix(block, "\n") {
			return Edit{}, fmt.Errorf("the managed Torana block was edited; review it before changing setup")
		}
		if !teardown {
			return Edit{Content: current}, nil
		}
		after := text[:start] + text[finish:]
		if _, err := toml.Decode(after, &parsed); err != nil {
			return Edit{}, fmt.Errorf("removal would invalidate Codex configuration")
		}
		return Edit{Content: []byte(after), Changed: true}, nil
	}
	if teardown {
		return Edit{Content: current}, nil
	}
	if servers, ok := parsed["mcp_servers"].(map[string]any); ok {
		if _, exists := servers["torana"]; exists {
			return Edit{}, fmt.Errorf("an unowned torana MCP server already exists; keep it or remove it explicitly")
		}
	}
	separator := "\n"
	if text != "" && !strings.HasSuffix(text, "\n") {
		separator = "\n\n"
	}
	after := text + separator + block
	if _, err := toml.Decode(after, &parsed); err != nil {
		return Edit{}, fmt.Errorf("Torana MCP entry conflicts with the existing Codex configuration")
	}
	return Edit{Content: []byte(after), Changed: true}, nil
}

// A normal map decoder silently drops duplicate JSON members. Never use it
// to rewrite a user's file without detecting that ambiguity first.
func uniqueJSONKeys(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var walk func(int) bool
	walk = func(depth int) bool {
		if depth > 64 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delimiter, container := token.(json.Delim)
		if !container {
			return true
		}
		if delimiter != '{' && delimiter != '[' {
			return false
		}
		seen := map[string]bool{}
		for decoder.More() {
			if delimiter == '{' {
				key, err := decoder.Token()
				if err != nil {
					return false
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return false
				}
				seen[name] = true
			}
			if !walk(depth + 1) {
				return false
			}
		}
		closing, err := decoder.Token()
		return err == nil && (delimiter == '{' && closing == json.Delim('}') || delimiter == '[' && closing == json.Delim(']'))
	}
	if !walk(0) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func codexBlock(server Server) (string, error) {
	var body bytes.Buffer
	config := struct {
		Servers map[string]Server `toml:"mcp_servers"`
	}{Servers: map[string]Server{"torana": {Command: server.Command, Args: server.Args}}}
	if err := toml.NewEncoder(&body).Encode(config); err != nil {
		return "", err
	}
	return begin + "\n" + body.String() + end + "\n", nil
}
