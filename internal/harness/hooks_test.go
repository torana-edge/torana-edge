package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeHooksPreserveUnrelatedSettingsAndOwnExactGroups(t *testing.T) {
	before := []byte(`{"permissions":{"allow":["Read"]},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"my-hook"}]}]}}`)
	installed, err := editClaudeHooks(before, nil, "http://127.0.0.1:8080", false, false)
	if err != nil || !installed.Changed || strings.Contains(string(installed.Content), "PreModelSwitch") {
		t.Fatalf("install=%+v %v", installed, err)
	}
	if !strings.Contains(string(installed.Content), "my-hook") || !strings.Contains(string(installed.Content), "$TORANA_MCP_TOKEN") {
		t.Fatal("lost unrelated hook or embedded a token instead of its env reference")
	}
	repeat, err := editClaudeHooks(installed.Content, installed.Ownership, "http://127.0.0.1:8080", false, false)
	if err != nil || repeat.Changed {
		t.Fatalf("repeat=%+v %v", repeat, err)
	}
	withPre, err := editClaudeHooks(installed.Content, installed.Ownership, "http://127.0.0.1:8080", true, false)
	if err != nil || !strings.Contains(string(withPre.Content), "PreModelSwitch") {
		t.Fatalf("optional=%+v %v", withPre, err)
	}
	withoutPre, err := editClaudeHooks(withPre.Content, withPre.Ownership, "http://127.0.0.1:8080", false, false)
	if err != nil || strings.Contains(string(withoutPre.Content), "PreModelSwitch") || !strings.Contains(string(withoutPre.Content), "PostModelSwitch") {
		t.Fatalf("remove optional dependency=%s %v", withoutPre.Content, err)
	}
	removed, err := editClaudeHooks(withPre.Content, withPre.Ownership, "http://127.0.0.1:8080", false, true)
	if err != nil || entryDigest(removed.Content) != entryDigest(before) {
		t.Fatalf("remove=%s %v", removed.Content, err)
	}
	edited := []byte(strings.ReplaceAll(string(installed.Content), "/stop", "/edited"))
	if _, err := editClaudeHooks(edited, installed.Ownership, "http://127.0.0.1:8080", false, true); err == nil {
		t.Fatal("edited hook was removed")
	}
	// An identical manually installed group must never become ours to remove.
	unowned, err := editClaudeHooks(installed.Content, nil, "http://127.0.0.1:8080", false, false)
	if err != nil || unowned.Changed {
		t.Fatalf("unowned repeat=%+v %v", unowned, err)
	}
	untouched, err := editClaudeHooks(installed.Content, nil, "http://127.0.0.1:8080", false, true)
	if err != nil || untouched.Changed {
		t.Fatal("unowned hook was changed")
	}
}

func TestClaudeHookPlanValidatesOriginAndStaleSettings(t *testing.T) {
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "settings.json")
	if _, err := PlanClaudeHooks(path, "https://example.com", false, false); err == nil {
		t.Fatal("remote hook could receive the MCP token")
	}
	plan, err := PlanClaudeHooks(path, "http://127.0.0.1:8080", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("preview wrote settings")
	}
	if err := os.WriteFile(path, []byte(`{"model":"user-choice"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Apply(); err == nil {
		t.Fatal("stale plan overwrote changed settings")
	}
	for _, invalid := range []string{`{"hooks":null}`, `{"hooks":{},"hooks":{}}`, `{"hooks":{"Stop":{}}}`} {
		if _, err := editClaudeHooks([]byte(invalid), nil, "http://127.0.0.1:8080", false, false); err == nil {
			t.Fatalf("invalid hooks accepted: %s", invalid)
		}
	}
	group, err := editClaudeHooks(nil, nil, "http://127.0.0.1:8080", true, false)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Timeout int `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(group.Content, &root) != nil || root.Hooks["PreModelSwitch"][0].Hooks[0].Timeout != 1 || root.Hooks["Stop"][0].Hooks[0].Timeout != 3 {
		t.Fatal("hook timeouts were not explicit")
	}
}
