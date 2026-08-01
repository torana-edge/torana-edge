package fixturebuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// repoRoot returns the torana-edge checkout root.
func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// newFixtureRoot creates a temp sandbox that stands in for the repo root: a
// fixture dir, a fake builder that logs every invocation, and a build log.
// The stamp engine's root detection falls back to $PWD outside a git tree, so
// the sandbox exercises the real script without touching the real repo.
func newFixtureRoot(t *testing.T) (root, fixtureDir, buildLog string) {
	t.Helper()
	root = t.TempDir()
	fixtureDir = filepath.Join(root, "examples", "plugins", "test-fake")
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	buildLog = filepath.Join(root, "build.log")

	builder := filepath.Join(root, "fake-builder.sh")
	script := "#!/bin/sh\n" +
		"echo \"build $(pwd)\" >> \"$BUILD_LOG\"\n" +
		"printf 'wasm' > plugin.wasm\n"
	if err := os.WriteFile(builder, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return root, fixtureDir, buildLog
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func buildCount(t *testing.T, buildLog string) int {
	t.Helper()
	b, err := os.ReadFile(buildLog)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return strings.Count(string(b), "build ")
}

// TestStampEngine covers the invalidation matrix with a logged fake builder:
// unchanged trees run zero builds, and only the right mutation triggers one.
func TestStampEngine(t *testing.T) {
	root, fixtureDir, buildLog := newFixtureRoot(t)
	writeFile(t, filepath.Join(fixtureDir, "plugin.wasm.go"), "package main\n")
	writeFile(t, filepath.Join(fixtureDir, "plugin.json"), "{}")

	// The real scripts dir lives at $root/.. only in the sandbox's view; the
	// script path must be the repo's script. Fix the path.
	repo := repoRoot(t)
	scriptPath := filepath.Join(repo, "scripts", "testdata.sh")

	run := func(t *testing.T, extraArg string) {
		t.Helper()
		stamp := filepath.Join(root, ".cache", "fixtures", "test-fake.stamp")
		out := filepath.Join(fixtureDir, "plugin.wasm")
		cmd := exec.Command("sh", scriptPath, fixtureDir, stamp, out, "--", filepath.Join(root, "fake-builder.sh"))
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "BUILD_LOG="+buildLog)
		if extraArg != "" {
			cmd.Args = append(cmd.Args, extraArg)
		}
		if outB, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("testdata.sh: %v\n%s", err, outB)
		}
	}

	t.Run("first build", func(t *testing.T) {
		run(t, "")
		if got := buildCount(t, buildLog); got != 1 {
			t.Fatalf("first build ran %d times, want 1", got)
		}
	})
	t.Run("unchanged rerun builds zero", func(t *testing.T) {
		run(t, "")
		if got := buildCount(t, buildLog); got != 1 {
			t.Fatalf("unchanged rerun ran %d builds, want still 1", got)
		}
	})
	t.Run("touch without content change builds zero", func(t *testing.T) {
		now := filepath.Join(fixtureDir, "plugin.wasm.go")
		if err := os.Chtimes(now, time.Now(), time.Now()); err != nil {
			t.Fatal(err)
		}
		run(t, "")
		if got := buildCount(t, buildLog); got != 1 {
			t.Fatalf("touch-only ran %d builds, want still 1 (content-addressed, not mtime)", got)
		}
	})
	t.Run("modification builds one", func(t *testing.T) {
		writeFile(t, filepath.Join(fixtureDir, "plugin.wasm.go"), "package main\n\n// changed\n")
		run(t, "")
		if got := buildCount(t, buildLog); got != 2 {
			t.Fatalf("modification ran %d builds, want 2", got)
		}
	})
	t.Run("addition builds one", func(t *testing.T) {
		writeFile(t, filepath.Join(fixtureDir, "extra.go"), "package main\n")
		run(t, "")
		if got := buildCount(t, buildLog); got != 3 {
			t.Fatalf("addition ran %d builds, want 3", got)
		}
	})
	t.Run("deletion builds one", func(t *testing.T) {
		if err := os.Remove(filepath.Join(fixtureDir, "extra.go")); err != nil {
			t.Fatal(err)
		}
		run(t, "")
		if got := buildCount(t, buildLog); got != 4 {
			t.Fatalf("deletion ran %d builds, want 4", got)
		}
	})
	t.Run("deleted output rebuilds itself", func(t *testing.T) {
		if err := os.Remove(filepath.Join(fixtureDir, "plugin.wasm")); err != nil {
			t.Fatal(err)
		}
		run(t, "")
		if got := buildCount(t, buildLog); got != 5 {
			t.Fatalf("deleted output ran %d builds, want 5", got)
		}
	})
	t.Run("go.mod change rebuilds", func(t *testing.T) {
		writeFile(t, filepath.Join(root, "go.mod"), "module sandbox\n")
		run(t, "")
		if got := buildCount(t, buildLog); got != 6 {
			t.Fatalf("go.mod addition ran %d builds, want 6", got)
		}
		writeFile(t, filepath.Join(root, "go.mod"), "module sandbox\n\n// changed\n")
		run(t, "")
		if got := buildCount(t, buildLog); got != 7 {
			t.Fatalf("go.mod change ran %d builds, want 7", got)
		}
	})
	t.Run("build-command change rebuilds", func(t *testing.T) {
		run(t, "-flag")
		if got := buildCount(t, buildLog); got != 8 {
			t.Fatalf("command change ran %d builds, want 8", got)
		}
	})
}

var (
	fullPathRef = regexp.MustCompile(`examples/plugins/[a-z0-9-]+`)
	dirRef      = regexp.MustCompile(`fixturesDir\+"/[a-z0-9-]+`)
)

// testdataDirs reads the Makefile TESTDATA_DIRS list.
func testdataDirs(t *testing.T) []string {
	t.Helper()
	repo := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(repo, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "TESTDATA_DIRS := ") {
			line = strings.TrimPrefix(l, "TESTDATA_DIRS := ")
			break
		}
	}
	if line == "" {
		t.Fatal("TESTDATA_DIRS not found in Makefile")
	}
	return strings.Fields(line)
}

// TestMakefileCoversEveryFixture — every Go fixture on disk is represented in
// the builder, and every listed entry exists.
func TestMakefileCoversEveryFixture(t *testing.T) {
	repo := repoRoot(t)
	dirs := testdataDirs(t)
	listed := map[string]bool{}
	for _, d := range dirs {
		if !strings.HasPrefix(d, "examples/plugins/") {
			continue
		}
		listed[d] = true
		if _, err := os.Stat(filepath.Join(repo, d, "plugin.wasm.go")); err != nil {
			t.Errorf("%s: listed in TESTDATA_DIRS but has no plugin.wasm.go", d)
		}
	}
	entries, err := os.ReadDir(filepath.Join(repo, "examples", "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join("examples", "plugins", e.Name())
		if _, err := os.Stat(filepath.Join(repo, dir, "plugin.wasm.go")); err != nil {
			continue // non-Go fixture (e.g. rust-redactor)
		}
		if !listed[dir] {
			t.Errorf("%s: has plugin.wasm.go but is not in Makefile TESTDATA_DIRS — a newly added guest that CI cannot build", dir)
		}
	}
}

// TestFixtureReferencesAreInventoried — every literal fixture reference in
// internal tests is covered by the builder, so a newly referenced guest cannot
// be forgotten.
func TestFixtureReferencesAreInventoried(t *testing.T) {
	repo := repoRoot(t)
	listed := map[string]bool{}
	for _, d := range testdataDirs(t) {
		listed[filepath.Base(d)] = true
	}

	err := filepath.WalkDir(filepath.Join(repo, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		for _, m := range fullPathRef.FindAllString(src, -1) {
			name := filepath.Base(m)
			if !listed[name] {
				t.Errorf("%s references %s, not in Makefile TESTDATA_DIRS", path, m)
			}
		}
		for _, m := range dirRef.FindAllString(src, -1) {
			name := strings.TrimPrefix(m, `fixturesDir+"/`)
			if !listed[name] {
				t.Errorf("%s references %s, not in Makefile TESTDATA_DIRS", path, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestFixturesForPkgMatchesReferences — the per-package fixture mapping (used
// by make test-pkg) is exactly the package's literal references; internal/wasm
// is special-cased to the full inventory.
func TestFixturesForPkgMatchesReferences(t *testing.T) {
	repo := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(repo, "internal"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pkg := "internal/" + e.Name()
		hasTests := false
		filepath.WalkDir(filepath.Join(repo, pkg), func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(path, "_test.go") {
				hasTests = true
			}
			return nil
		})
		if !hasTests {
			continue
		}
		t.Run(pkg, func(t *testing.T) {
			cmd := exec.Command("sh", filepath.Join(repo, "scripts", "fixtures-for-pkg.sh"), pkg)
			cmd.Dir = repo // the script resolves internal/... relative to the checkout root
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("fixtures-for-pkg.sh: %v", err)
			}
			got := map[string]bool{}
			for _, f := range strings.Fields(string(out)) {
				got[f] = true
			}

			want := map[string]bool{}
			if pkg == "internal/wasm" {
				for _, d := range testdataDirs(t) {
					if strings.HasPrefix(d, "examples/plugins/") {
						want[filepath.Base(d)] = true
					}
				}
			} else {
				filepath.WalkDir(filepath.Join(repo, pkg), func(path string, d os.DirEntry, err error) error {
					if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
						return nil
					}
					b, err := os.ReadFile(path)
					if err != nil {
						t.Error(err)
						return nil
					}
					src := string(b)
					for _, m := range fullPathRef.FindAllString(src, -1) {
						want[filepath.Base(m)] = true
					}
					for _, m := range dirRef.FindAllString(src, -1) {
						want[strings.TrimPrefix(m, `fixturesDir+"/`)] = true
					}
					return nil
				})
			}
			if len(got) != len(want) {
				t.Fatalf("mapping has %d fixtures, literal references have %d\nscript-only: %v\nref-only: %v",
					len(got), len(want), diff(got, want), diff(want, got))
			}
			for f := range want {
				if !got[f] {
					t.Errorf("fixture %s missing from the script mapping", f)
				}
			}
		})
	}
}

func diff(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	return out
}
