package harnesscmd

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/harness"
)

func TestClaudeServerInspectionStreamsLargeHistory(t *testing.T) {
	entry := `{"command":"torana","args":["mcp","stdio"]}`
	config := `{"history":["` + strings.Repeat("x", 2<<20) + `"],"mcpServers":{"other":{"args":[]},"torana":` + entry + `},"projects":{}}`
	got, err := readClaudeServer(strings.NewReader(config))
	if err != nil || string(got) != entry {
		t.Fatalf("entry=%s error=%v", got, err)
	}
	for _, invalid := range []string{
		`{"mcpServers":{},"mcpServers":{}}`,
		`{"mcpServers":{"torana":{},"torana":{}}}`,
		`{"mcpServers":null}`,
		`{"mcpServers":{"torana":{}}} {}`,
		`{"history":[1,`,
	} {
		if _, err := readClaudeServer(strings.NewReader(invalid)); err == nil {
			t.Fatalf("accepted invalid configuration: %s", invalid)
		}
	}
}

func TestNativeCodexPreviewInstallRepeatAndOwnedRemoval(t *testing.T) {
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	server := harness.Server{Command: "torana", Args: []string{"mcp", "stdio"}}
	var entry json.RawMessage
	var mutations [][]string
	run := func(_ string, args ...string) ([]byte, error) {
		switch args[1] {
		case "get":
			if entry == nil {
				return []byte("No MCP server named 'torana' found."), errors.New("not found")
			}
			return json.Marshal(map[string]any{"transport": entry, "enabled": true})
		case "add":
			mutations = append(mutations, append([]string(nil), args...))
			entry = json.RawMessage(`{"type":"stdio","command":"torana","args":["mcp","stdio"],"env":null,"env_vars":[],"cwd":null}`)
		case "remove":
			mutations = append(mutations, append([]string(nil), args...))
			entry = nil
		}
		return nil, nil
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	plan, err := planNative("codex", "codex", "user", path, server, false, run)
	if err != nil || !plan.changed || len(mutations) != 0 {
		t.Fatalf("preview=%+v %v", plan, err)
	}
	if err := plan.apply(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(mutations[0], []string{"mcp", "add", "torana", "--", "torana", "mcp", "stdio"}) {
		t.Fatal(mutations)
	}
	repeat, err := planNative("codex", "codex", "user", path, server, false, run)
	if err != nil || repeat.changed {
		t.Fatalf("repeat=%+v %v", repeat, err)
	}
	remove, err := planNative("codex", "codex", "user", path, harness.Server{Command: "moved"}, true, run)
	if err != nil || !remove.changed {
		t.Fatalf("remove=%+v %v", remove, err)
	}
	if err := remove.apply(); err != nil {
		t.Fatal(err)
	}
	if entry != nil || len(mutations) != 2 {
		t.Fatal("native removal did not run")
	}
}

func TestNativeRefusesInspectionErrorsUnownedAndExtraEnvironment(t *testing.T) {
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	server := harness.Server{Command: "torana", Args: []string{"mcp", "stdio"}}
	for _, body := range []string{
		`{"transport":{"type":"stdio","command":"torana","args":["mcp","stdio"]}}`,
		`{"transport":{"type":"stdio","command":"torana","args":["mcp","stdio"],"env":{"TOKEN":"private"}}}`,
		`{"transport":{"type":"stdio","command":"other","args":[]}}`,
	} {
		run := func(_ string, _ ...string) ([]byte, error) { return []byte(body), nil }
		if _, err := planNative("codex", "codex", "user", filepath.Join(t.TempDir(), "config"), server, true, run); err == nil {
			t.Fatal("unowned/modified server was removable")
		}
	}
	run := func(_ string, _ ...string) ([]byte, error) { return []byte("private diagnostic"), errors.New("failed") }
	if _, err := planNative("codex", "codex", "user", "config", server, false, run); err == nil || err.Error() == "private diagnostic" {
		t.Fatal("inspection failure mistaken for missing entry")
	}
}
