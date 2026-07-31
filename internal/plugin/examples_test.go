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

	// Two fixtures are deliberately NOT installable: they are ABI v1 smoke
	// fixtures for the Rust and AssemblyScript trampolines, excluded from
	// TESTDATA_DIRS and never built. This host is v2-only, so their manifests
	// are correctly rejected. They are named here rather than skipped by a
	// pattern, so adding a third one is a deliberate act someone has to
	// justify — during the migration a bulk edit relabelled both as v2 and
	// nothing noticed.
	notInstallable := map[string]string{
		"rust-redactor": "ABI v1 smoke fixture; port when the Rust SDK moves to v2",
		"ts-logger":     "ABI v1 smoke fixture (AssemblyScript)",
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
		if reason, excluded := notInstallable[e.Name()]; excluded {
			// Assert it really is what we claim, so the exclusion cannot
			// silently cover a v2 fixture that has broken.
			t.Run(e.Name()+"_excluded", func(t *testing.T) {
				if _, err := ValidateManifestDir(dir); err == nil {
					t.Errorf("%s is excluded as %q but now validates — "+
						"remove it from notInstallable", e.Name(), reason)
				}
			})
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
