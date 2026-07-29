package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

// Every example manifest must pass the same validation an installed plugin
// does.
//
// Eleven of them shipped the pre-v1 shape — no schema_version, id, abi_version
// or failure_mode — and nothing noticed, because the tests that load them set
// AllowUnapproved, which skips validation entirely. So the examples were
// simultaneously the first thing an author copies and the one bundle shape the
// host rejects:
//
//	$ torana plugin install ./my-plugin
//	unsupported schema_version 0
//
// with no hint as to which field is missing. Validating them here means the
// examples fail in CI rather than in someone's terminal on their first attempt.
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
