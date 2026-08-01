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
		src := filepath.Join(fixtureDir, "plugin.wasm.go")
		origInfo, err := os.Stat(src)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, src, "package main\n\n// changed\n")
		// Restore the ORIGINAL mtime: a content change must rebuild even when
		// Make's timestamps would consider the output current.
		if err := os.Chtimes(src, origInfo.ModTime(), origInfo.ModTime()); err != nil {
			t.Fatal(err)
		}
		run(t, "")
		if got := buildCount(t, buildLog); got != 2 {
			t.Fatalf("modification with a restored mtime ran %d builds, want 2", got)
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
	t.Run("deleted stamp reconstructs and rebuilds", func(t *testing.T) {
		stamp := filepath.Join(root, ".cache", "fixtures", "test-fake.stamp")
		if err := os.Remove(stamp); err != nil {
			t.Fatal(err)
		}
		run(t, "")
		if got := buildCount(t, buildLog); got != 9 {
			t.Fatalf("deleted stamp ran %d builds, want 9 (the stamp is the fingerprint record)", got)
		}
	})
	// Created on the PARENT test: t.TempDir() inside the subtest would delete
	// the fake go when the subtest returns, and the identical-run subtest
	// would silently fall back to the real toolchain.
	fakeDir := t.TempDir()
	t.Run("toolchain version change rebuilds", func(t *testing.T) {
		// A fake `go` ahead of PATH changes only the reported version: the
		// build command is the sandbox's fake builder, so this isolates the
		// go version line of the fingerprint.
		fakeGo := `#!/bin/sh
if [ "$1" = version ]; then echo 'go version go9.9.9 fake'; else exec /usr/bin/env go "$@"; fi
`
		if err := os.WriteFile(filepath.Join(fakeDir, "go"), []byte(fakeGo), 0o755); err != nil {
			t.Fatal(err)
		}
		stamp := filepath.Join(root, ".cache", "fixtures", "test-fake.stamp")
		out := filepath.Join(fixtureDir, "plugin.wasm")
		cmd := exec.Command("sh", scriptPath, fixtureDir, stamp, out, "--", filepath.Join(root, "fake-builder.sh"))
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "BUILD_LOG="+buildLog, "PATH="+fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		if outB, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("testdata.sh: %v\n%s", err, outB)
		}
		if got := buildCount(t, buildLog); got != 10 {
			t.Fatalf("toolchain change ran %d builds, want 10 (go version is part of the fingerprint)", got)
		}
	})
	t.Run("identical run after version change builds zero", func(t *testing.T) {
		// Same toolchain as the previous subtest: the fingerprint is
		// stable again, so no rebuild.
		stamp := filepath.Join(root, ".cache", "fixtures", "test-fake.stamp")
		out := filepath.Join(fixtureDir, "plugin.wasm")
		cmd := exec.Command("sh", scriptPath, fixtureDir, stamp, out, "--", filepath.Join(root, "fake-builder.sh"))
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "BUILD_LOG="+buildLog, "PATH="+fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		if outB, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("testdata.sh: %v\n%s", err, outB)
		}
		if got := buildCount(t, buildLog); got != 10 {
			t.Fatalf("unchanged rerun after version change ran %d builds, want still 10", got)
		}
	})
}

// TestShasumFallback — the portable hash helper must work when sha256sum is
// unavailable (macOS): TESTDATA_HASH_TOOL=shasum forces the fallback, with a
// fake shasum standing in for the macOS tool.
func TestShasumFallback(t *testing.T) {
	root, fixtureDir, buildLog := newFixtureRoot(t)
	writeFile(t, filepath.Join(fixtureDir, "plugin.wasm.go"), "package main\n")

	fakeDir := t.TempDir()
	fakeShasum := "#!/bin/sh\nif [ \"$1\" = -a ] && [ \"$2\" = 256 ]; then shift 2; fi\nexec /usr/bin/env sha256sum \"$@\"\n"
	if err := os.WriteFile(filepath.Join(fakeDir, "shasum"), []byte(fakeShasum), 0o755); err != nil {
		t.Fatal(err)
	}

	runWith := func(t *testing.T) {
		t.Helper()
		stamp := filepath.Join(root, ".cache", "fixtures", "test-fake.stamp")
		out := filepath.Join(fixtureDir, "plugin.wasm")
		cmd := exec.Command("sh", filepath.Join(repoRoot(t), "scripts", "testdata.sh"),
			fixtureDir, stamp, out, "--", filepath.Join(root, "fake-builder.sh"))
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "BUILD_LOG="+buildLog,
			"TESTDATA_HASH_TOOL=shasum", "PATH="+fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		if outB, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("testdata.sh (shasum): %v\n%s", err, outB)
		}
	}
	runWith(t)
	if got := buildCount(t, buildLog); got != 1 {
		t.Fatalf("shasum fallback built %d times, want 1", got)
	}
	runWith(t)
	if got := buildCount(t, buildLog); got != 1 {
		t.Fatalf("shasum fallback rerun built %d times, want still 1", got)
	}
}

// TestMakeForceDesignRunsTheFingerprintEveryTime — at the MAKE level, the
// per-fixture recipe always executes (force-fixtures prerequisite); the
// fingerprint script is the sole go-build decision. An up-to-date output must
// produce zero builds on every invocation.
func TestMakeForceDesignRunsTheFingerprintEveryTime(t *testing.T) {
	repo := repoRoot(t)
	target := "examples/plugins/test-observer/plugin.wasm"
	for i := 0; i < 2; i++ {
		cmd := exec.Command("make", target)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("make %s: %v\n%s", target, err, out)
		}
		if strings.Contains(string(out), "building ") {
			t.Fatalf("run %d: an up-to-date fixture was rebuilt:\n%s", i+1, out)
		}
	}
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

// fixtureNames reads the Makefile TESTDATA_DIRS list (bare names).
func fixtureNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, d := range testdataDirs(t) {
		if strings.HasPrefix(d, "examples/plugins/") {
			names = append(names, filepath.Base(d))
		}
	}
	return names
}

// runMapper runs the real AST mapper against a package path.
func runMapper(t *testing.T, repo, pkg string) map[string]bool {
	t.Helper()
	cmd := exec.Command("go", "run", "./scripts/fixtures-for-pkg.go", pkg)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixtures-for-pkg.go %s: %v\n%s", pkg, err, out)
	}
	got := map[string]bool{}
	for _, f := range strings.Fields(string(out)) {
		got[f] = true
	}
	return got
}

// TestMapperCatchesDynamicNames — the exact dynamically constructed
// references the path-grep mapping missed: table/argument values such as
// blockE2E(..., []string{"test-blocker"}) and fixturesDir+"/"+name shapes.
func TestMapperCatchesDynamicNames(t *testing.T) {
	repo := repoRoot(t)
	got := runMapper(t, repo, "internal/proxy")
	for _, want := range []string{"test-blocker", "test-blocker-nogrant", "test-responder", "test-responder-nogrant", "test-slow-after-stream"} {
		if !got[want] {
			t.Errorf("proxy mapping missing %s — a strict package run would fail", want)
		}
	}
}

// TestMapperOutputIsKnownFixtures — the mapping may over-include (safe) but
// never invent names outside the builder's inventory.
func TestMapperOutputIsKnownFixtures(t *testing.T) {
	repo := repoRoot(t)
	known := map[string]bool{}
	for _, n := range fixtureNames(t) {
		known[n] = true
	}
	for _, pkg := range []string{"internal/proxy", "internal/plugin"} {
		for f := range runMapper(t, repo, pkg) {
			if !known[f] {
				t.Errorf("%s: mapper produced %s, not in TESTDATA_DIRS", pkg, f)
			}
		}
	}
}

// TestMapperCoversLiteralFullPaths — every literal examples/plugins/<name>
// reference in a package is present in its mapping (the strong direction of
// the old equality test, without reimplementing the mapper).
func TestMapperCoversLiteralFullPaths(t *testing.T) {
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
		if pkg == "internal/wasm" {
			continue // special-cased to the full inventory
		}
		got := runMapper(t, repo, pkg)
		filepath.WalkDir(filepath.Join(repo, pkg), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Error(err)
				return nil
			}
			for _, m := range fullPathRef.FindAllString(string(b), -1) {
				if !got[filepath.Base(m)] {
					t.Errorf("%s references %s but its fixture mapping omits it", path, m)
				}
			}
			return nil
		})
	}
}

// TestMapperSyntheticPackage — the AST logic directly: a temp package whose
// tests reference fixtures only through table values and path concatenation
// must map them all.
func TestMapperSyntheticPackage(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	src := "package synth\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {\n" +
		"\tfor _, name := range []string{\"test-blocker\", \"test-responder\"} {\n" +
		"\t\t_ = name\n\t}\n" +
		"\t_ = `fixturesDir+\"/test-observer/plugin.wasm\"`\n}\n"
	if err := os.WriteFile(filepath.Join(tmp, "synth_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runMapper(t, repo, tmp)
	for _, want := range []string{"test-blocker", "test-responder", "test-observer"} {
		if !got[want] {
			t.Errorf("mapper missed %q from the synthetic package", want)
		}
	}
}

// TestWasmShardIsTheFullInventory — internal/wasm's all-fixture ABI inventory
// requires every v2 fixture.
func TestWasmShardIsTheFullInventory(t *testing.T) {
	repo := repoRoot(t)
	got := runMapper(t, repo, "internal/wasm")
	want := fixtureNames(t)
	if len(got) != len(want) {
		t.Fatalf("wasm mapping has %d fixtures, want %d", len(got), len(want))
	}
	for _, n := range want {
		if !got[n] {
			t.Errorf("wasm mapping missing %s", n)
		}
	}
}

// TestCIShardsCheck — the single-source shard partition rejects duplicates
// and corruptions. The helper is the SAME script the workflow consumes, so a
// drift between the matrix and the proof is impossible by construction.
func TestCIShardsCheck(t *testing.T) {
	repo := repoRoot(t)
	script := filepath.Join(repo, "scripts", "ci-shards.sh")
	run := func(t *testing.T, input string) error {
		t.Helper()
		cmd := exec.Command("sh", script, "check-synthetic")
		cmd.Dir = repo
		cmd.Stdin = strings.NewReader(input)
		return cmd.Run()
	}

	realish := `github.com/torana-edge/torana-edge
github.com/torana-edge/torana-edge/internal/wasm
github.com/torana-edge/torana-edge/internal/wasm/z
github.com/torana-edge/torana-edge/internal/plugin
github.com/torana-edge/torana-edge/internal/plugincmd
github.com/torana-edge/torana-edge/internal/proxy
github.com/torana-edge/torana-edge/internal/secret
github.com/torana-edge/torana-edge/test/e2e
`

	t.Run("well-formed partition passes", func(t *testing.T) {
		if err := run(t, realish); err != nil {
			t.Fatalf("check rejected a well-formed inventory: %v", err)
		}
	})
	t.Run("a deliberately duplicated package fails", func(t *testing.T) {
		dup := realish + "github.com/torana-edge/torana-edge/internal/wasm\n"
		if err := run(t, dup); err == nil {
			t.Fatal("check accepted a duplicated package")
		}
	})
	t.Run("an overlapped partition fails (revert-proof)", func(t *testing.T) {
		// Corrupt the helper: make the plugin pattern also match the wasm
		// package, then restore.
		orig, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		corrupt := strings.Replace(string(orig),
			"PLUGIN_MOD='^github.com/torana-edge/torana-edge/internal/plugin(/|$)'",
			"PLUGIN_MOD='^github.com/torana-edge/torana-edge/internal/(plugin|wasm)(/|$)'", 1)
		if err := os.WriteFile(script, []byte(corrupt), 0o755); err != nil {
			t.Fatal(err)
		}
		err = run(t, realish)
		os.WriteFile(script, orig, 0o755)
		if err == nil {
			t.Fatal("check accepted an overlapping partition")
		}
	})
	t.Run("an omitted package fails (revert-proof)", func(t *testing.T) {
		// Corrupt the remainder exclusion by dropping the anchor: internal/
		// plugincmd then matches the exclusion but no shard pattern, so it is
		// omitted from the union.
		orig, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		corrupt := strings.Replace(string(orig),
			"SHARD_MOD='^github.com/torana-edge/torana-edge/internal/(wasm|plugin|proxy)(/|$)'",
			"SHARD_MOD='^github.com/torana-edge/torana-edge/internal/(wasm|plugin|proxy)'", 1)
		if err := os.WriteFile(script, []byte(corrupt), 0o755); err != nil {
			t.Fatal(err)
		}
		err = run(t, realish)
		os.WriteFile(script, orig, 0o755)
		if err == nil {
			t.Fatal("check accepted a partition that omits a package")
		}
	})
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
