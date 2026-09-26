package harness

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestFilePlanPreviewBackupIdempotencyAndNarrowTeardown(t *testing.T) {
	for _, name := range []string{"claude-code", "codex"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			before := []byte("model = 'keep-me'\n")
			if name == "claude-code" {
				before = []byte(`{"other":"keep-me"}`)
			}
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			plan, err := PlanFile(path, name, testServer(), false)
			if err != nil {
				t.Fatal(err)
			}
			actual, _ := os.ReadFile(path)
			if !bytes.Equal(actual, before) {
				t.Fatal("preview mutated configuration")
			}
			backup, err := plan.Apply()
			if err != nil {
				t.Fatal(err)
			}
			recovery, _ := os.ReadFile(backup)
			if !bytes.Equal(recovery, before) {
				t.Fatal("backup lost original bytes")
			}
			info, _ := os.Stat(backup)
			if info.Mode().Perm() != 0o600 {
				t.Fatal("backup permissions expose credentials")
			}
			again, err := PlanFile(path, name, testServer(), false)
			if err != nil || again.Changed {
				t.Fatalf("repeat=%+v err=%v", again, err)
			}
			remove, err := PlanFile(path, name, testServer(), true)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := remove.Apply(); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Contains(after, []byte("keep-me")) {
				t.Fatal("teardown lost unrelated state")
			}
		})
	}
}

func TestFilePlanRefusesStalePreviewAndSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	plan, err := PlanFile(path, "claude-code", testServer(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"new":"user-edit"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Apply(); err == nil {
		t.Fatal("stale preview overwrote user edit")
	}
	link := filepath.Join(filepath.Dir(path), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanFile(link, "claude-code", testServer(), false); err == nil {
		t.Fatal("followed symlink")
	}
}
