package harness

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func testServer() Server {
	return Server{Command: "/Applications/Torana Tools/torana", Args: []string{"mcp", "stdio", "--addr", "127.0.0.1:8080"}}
}

func TestClaudeSetupPreservesOtherServersAndState(t *testing.T) {
	before := []byte(`{"mcpServers":{"other":{"command":"other","env":{"TOKEN":"private"}}},"onboarding":true}`)
	edit, err := EditClaude(before, nil, testServer(), false)
	if err != nil || !edit.Changed {
		t.Fatalf("edit=%+v err=%v", edit, err)
	}
	var root map[string]any
	_ = json.Unmarshal(edit.Content, &root)
	if root["onboarding"] != true || root["mcpServers"].(map[string]any)["other"].(map[string]any)["env"].(map[string]any)["TOKEN"] != "private" {
		t.Fatal("unrelated Claude state changed")
	}
	again, err := EditClaude(edit.Content, edit.Ownership, testServer(), false)
	if err != nil || again.Changed || !bytes.Equal(again.Content, edit.Content) {
		t.Fatal("setup not idempotent")
	}
	removed, err := EditClaude(edit.Content, edit.Ownership, testServer(), true)
	if err != nil || !removed.Changed {
		t.Fatalf("teardown=%+v err=%v", removed, err)
	}
	_ = json.Unmarshal(removed.Content, &root)
	if len(root["mcpServers"].(map[string]any)) != 1 || root["mcpServers"].(map[string]any)["other"] == nil || root["onboarding"] != true {
		t.Fatal("teardown removed unrelated state")
	}
	if _, err := EditClaude(edit.Content, nil, testServer(), true); err == nil {
		t.Fatal("removed unowned entry")
	}
	changed := bytes.Replace(edit.Content, []byte("127.0.0.1:8080"), []byte("127.0.0.1:9090"), 1)
	if _, err := EditClaude(changed, edit.Ownership, testServer(), true); err == nil {
		t.Fatal("removed edited entry")
	}
}

func TestClaudeRefusesConflictsAndInvalidContainers(t *testing.T) {
	for _, current := range []string{`null`, `[]`, `{"mcpServers":null}`, `{"mcpServers":[]}`, `{"mcpServers":{"torana":{"command":"other"}}}`, `{"mcpServers":{},"mcpServers":{}}`, `{"other":{"token":1,"token":2}}`} {
		if _, err := EditClaude([]byte(current), nil, testServer(), false); err == nil {
			t.Fatalf("accepted %s", current)
		}
	}
}

func TestCodexSetupPreservesBytesAndEmitsSupportedFields(t *testing.T) {
	before := []byte("# keep my comments\nmodel = 'my-model'\n[mcp_servers.other]\ncommand = 'other'\n")
	edit, err := EditCodex(before, testServer(), false)
	if err != nil || !edit.Changed || !bytes.HasPrefix(edit.Content, before) {
		t.Fatalf("edit=%+v err=%v", edit, err)
	}
	var root map[string]any
	if _, err := toml.Decode(string(edit.Content), &root); err != nil {
		t.Fatal(err)
	}
	server := root["mcp_servers"].(map[string]any)["torana"].(map[string]any)
	if server["command"] != testServer().Command || server["args"] == nil || len(server) != 2 {
		t.Fatalf("server=%+v", server)
	}
	again, err := EditCodex(edit.Content, testServer(), false)
	if err != nil || again.Changed || !bytes.Equal(again.Content, edit.Content) {
		t.Fatal("setup not idempotent")
	}
	removed, err := EditCodex(edit.Content, testServer(), true)
	if err != nil || !removed.Changed || !bytes.HasPrefix(removed.Content, before) || strings.Contains(string(removed.Content), "mcp_servers.torana") {
		t.Fatalf("teardown=%+v err=%v", removed, err)
	}
	changed := bytes.Replace(edit.Content, []byte("127.0.0.1:8080"), []byte("127.0.0.1:9090"), 1)
	if _, err := EditCodex(changed, testServer(), true); err == nil {
		t.Fatal("removed edited block")
	}
}

func TestCodexRefusesQuotedInlineConflictsAndAmbiguousOwnership(t *testing.T) {
	for _, current := range []string{"[mcp_servers.'torana']\ncommand = 'other'", `mcp_servers = { torana = { command = "other" } }`, begin, end, "invalid = [", begin + "\n" + end + "\n" + begin + "\n" + end} {
		if _, err := EditCodex([]byte(current), testServer(), false); err == nil {
			t.Fatalf("accepted %q", current)
		}
	}
}
