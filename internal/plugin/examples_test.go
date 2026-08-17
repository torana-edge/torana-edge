package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

// Every shipped example manifest must pass the same validation as an installed
// plugin. This keeps copyable examples installable and makes a new invalid
// fixture fail in CI rather than in an author's first local run.
func TestExampleManifestsValidate(t *testing.T) {
	entries, err := os.ReadDir(fixturesDir)
	if err != nil {
		t.Fatalf("read %s: %v", fixturesDir, err)
	}

	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(fixturesDir, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "plugin.json")); err != nil {
			continue
		}
		checked++
		t.Run(e.Name(), func(t *testing.T) {
			if _, err := ValidateManifestDir(dir); err != nil {
				t.Errorf("%s would be rejected at install: %v", e.Name(), err)
			}
		})
	}

	// A glob that silently matches nothing is the failure mode this whole file
	// exists to prevent.
	if checked == 0 {
		t.Fatalf("no example manifests found under %s — has the directory moved?", fixturesDir)
	}
}
