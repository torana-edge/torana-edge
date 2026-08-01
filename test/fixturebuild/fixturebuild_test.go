package fixturebuild

import (
	"fmt"
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

// runMapper runs the PRODUCTION wrapper (GOWORK=off inside) against one or
// more package paths — the same entry point make and CI consume.
func runMapper(t *testing.T, repo string, pkgs ...string) map[string]bool {
	t.Helper()
	cmd := exec.Command("sh", filepath.Join(repo, "scripts", "fixtures-for-pkg.sh"))
	cmd.Args = append(cmd.Args, pkgs...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixtures-for-pkg %v: %v\n%s", pkgs, err, out)
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

// TestCIShardsCheck — the single-source shard partition rejects duplicates,
// overlaps, omissions, empty inventories, and failed listings. The script is
// COPIED into a temp sandbox before any corruption, so a broken test can never
// damage the tracked checkout.
func TestCIShardsCheck(t *testing.T) {
	repo := repoRoot(t)
	sandbox := t.TempDir()
	scriptsDir := filepath.Join(sandbox, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	orig, err := os.ReadFile(filepath.Join(repo, "scripts", "ci-shards.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(scriptsDir, "ci-shards.sh")
	writeScript := func(t *testing.T, content []byte) {
		t.Helper()
		if err := os.WriteFile(script, content, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeScript(t, orig)

	run := func(t *testing.T, input string) error {
		t.Helper()
		cmd := exec.Command("sh", script, "check-synthetic")
		cmd.Dir = sandbox
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
	t.Run("empty inventory fails", func(t *testing.T) {
		if err := run(t, ""); err == nil {
			t.Fatal("check accepted an empty inventory")
		}
	})
	t.Run("a failing go list fails the check", func(t *testing.T) {
		// The sandbox copy has no Go module: check mode's go list fails, and
		// that failure must propagate instead of verifying an empty list.
		cmd := exec.Command("sh", script, "check")
		cmd.Dir = sandbox
		if err := cmd.Run(); err == nil {
			t.Fatal("check succeeded despite a failing go list")
		}
	})
	t.Run("a PARTIALLY failing go list fails every mode", func(t *testing.T) {
		// A fake go that emits one valid package and then exits non-zero: a
		// partial listing must not surface as a successful partial shard or
		// a verified partition.
		fakeDir := t.TempDir()
		fakeGo := "#!/bin/sh\nprintf 'github.com/torana-edge/torana-edge/internal/wasm\\n'\nexit 1\n"
		if err := os.WriteFile(filepath.Join(fakeDir, "go"), []byte(fakeGo), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"wasm", "remainder", "check"} {
			cmd := exec.Command("sh", script, mode)
			cmd.Dir = sandbox
			cmd.Env = append(os.Environ(), "PATH="+fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("%s succeeded with a partially failing go list:\n%s", mode, out)
			}
			// No PARTIAL AUTHORITATIVE OUTPUT: the error message may appear,
			// but never a package line pretending to be a shard.
			if strings.Contains(string(out), "github.com/") {
				t.Fatalf("%s emitted partial package output despite the failure:\n%s", mode, out)
			}
		}
	})
	t.Run("an overlapped partition fails (revert-proof)", func(t *testing.T) {
		corrupt := strings.Replace(string(orig),
			"PLUGIN_MOD='^github.com/torana-edge/torana-edge/internal/plugin(/|$)'",
			"PLUGIN_MOD='^github.com/torana-edge/torana-edge/internal/(plugin|wasm)(/|$)'", 1)
		writeScript(t, []byte(corrupt))
		if err := run(t, realish); err == nil {
			t.Fatal("check accepted an overlapping partition")
		}
		writeScript(t, orig)
	})
	t.Run("an omitted package fails (revert-proof)", func(t *testing.T) {
		corrupt := strings.Replace(string(orig),
			"SHARD_MOD='^github.com/torana-edge/torana-edge/internal/(wasm|plugin|proxy)(/|$)'",
			"SHARD_MOD='^github.com/torana-edge/torana-edge/internal/(wasm|plugin|proxy)'", 1)
		writeScript(t, []byte(corrupt))
		if err := run(t, realish); err == nil {
			t.Fatal("check accepted a partition that omits a package")
		}
		writeScript(t, orig)
	})
}

// TestMakeForceDesignRunsTheFingerprintEveryTime — at the MAKE level, the
// force pattern (REAL fingerprint script, fake builder) always executes the
// recipe; the fingerprint is the sole go-build decision. An up-to-date output
// must produce zero builds on every invocation, and a content change with a
// restored old mtime must produce exactly one. The sandbox Makefile keeps the
// proof hermetic: no repository fixture, stamp, or output is touched.
func TestMakeForceDesignRunsTheFingerprintEveryTime(t *testing.T) {
	repo := repoRoot(t)
	sandbox := t.TempDir()
	srcDir := filepath.Join(sandbox, "src")
	outDir := filepath.Join(sandbox, "out")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(srcDir, "plugin.wasm.go"), "package main\n")
	writeFile(t, filepath.Join(srcDir, "plugin.json"), "{}")

	builder := filepath.Join(sandbox, "fake-builder.sh")
	builderSrc := "#!/bin/sh\necho \"build $(pwd)\" >> \"$BUILD_LOG\"\nprintf wasm > plugin.wasm\n"
	if err := os.WriteFile(builder, []byte(builderSrc), 0o755); err != nil {
		t.Fatal(err)
	}

	mk := filepath.Join(sandbox, "Makefile")
	// The target IS the builder's output (the fake builder writes plugin.wasm
	// into its working directory, exactly like the real go build does).
	mkSrc := ".PHONY: force-fixtures\n" +
		"src/plugin.wasm: force-fixtures src/plugin.wasm.go\n" +
		"\t@sh " + filepath.Join(repo, "scripts", "testdata.sh") +
		" src .stamp $@ -- " + builder + "\n"
	if err := os.WriteFile(mk, []byte(mkSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	runMake := func(t *testing.T) string {
		t.Helper()
		cmd := exec.Command("make", "-f", mk, "src/plugin.wasm")
		cmd.Dir = sandbox
		cmd.Env = append(os.Environ(), "BUILD_LOG="+filepath.Join(sandbox, "build.log"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("make: %v\n%s", err, out)
		}
		return string(out)
	}

	// First run builds the sandbox output; the two runs after it must not.
	if out := runMake(t); !strings.Contains(out, "building ") {
		t.Fatalf("the first run did not build the sandbox output:\n%s", out)
	}
	for i := 2; i <= 3; i++ {
		if out := runMake(t); strings.Contains(out, "building ") {
			t.Fatalf("run %d: an up-to-date fixture was rebuilt:\n%s", i, out)
		}
	}
	src := filepath.Join(srcDir, "plugin.wasm.go")
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, src, "package main\n\n// changed\n")
	if err := os.Chtimes(src, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if out := runMake(t); !strings.Contains(out, "building ") {
		t.Fatalf("a content change with a restored mtime did not rebuild:\n%s", out)
	}
}

// TestHostileGoWorkIgnored — a parent or environment go.work must not change
// which module the fixture builds or the mapper use: GOWORK=off is part of
// WASM_BUILD, the mapper wrapper, and the CI helper invocations.
func TestHostileGoWorkIgnored(t *testing.T) {
	repo := repoRoot(t)
	hostile := filepath.Join(t.TempDir(), "go.work")
	writeFile(t, hostile, "go 1.99\nuse ../../nonexistent/module\n")

	// The mapper wrapper must ignore the hostile workspace.
	cmd := exec.Command("sh", filepath.Join(repo, "scripts", "fixtures-for-pkg.sh"), "internal/proxy")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GOWORK="+hostile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mapper failed under a hostile GOWORK: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "test-observer") {
		t.Fatalf("mapper output incomplete under a hostile GOWORK:\n%s", out)
	}

	// The CI helper's fixture query (the same entry point CI consumes) must
	// also ignore the hostile workspace.
	scmd := exec.Command("sh", filepath.Join(repo, "scripts", "ci-shards.sh"), "fixtures", "proxy")
	scmd.Dir = repo
	scmd.Env = append(os.Environ(), "GOWORK="+hostile)
	if outB, err := scmd.CombinedOutput(); err != nil {
		t.Fatalf("ci-shards.sh fixtures under a hostile GOWORK: %v\n%s", err, outB)
	} else if !strings.Contains(string(outB), "examples/plugins/test-observer/plugin.wasm") {
		t.Fatalf("ci-shards.sh fixtures output incomplete:\n%s", outB)
	}

	// WASM_BUILD carries GOWORK=off so guest builds are equally insulated.
	mk, err := os.ReadFile(filepath.Join(repo, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(mk), "\n") {
		if strings.HasPrefix(line, "WASM_BUILD = ") {
			if !strings.Contains(line, "GOWORK=off") {
				t.Fatalf("WASM_BUILD lacks GOWORK=off: %s", line)
			}
		}
	}
}

// TestCacheDirOverride — the EFFECTIVE cache path semantics, tested through
// the repository's own cache-dir target: absolute overrides pass through
// unchanged, relative overrides are anchored at the repo root (every Go test
// process would otherwise resolve them from a different directory), and the
// default is the ignored repo-local dir. The target must not create any
// cache directory.
func TestCacheDirOverride(t *testing.T) {
	repo := repoRoot(t)
	effective := func(t *testing.T, args ...string) string {
		t.Helper()
		cmd := exec.Command("make", append([]string{"-s", "cache-dir"}, args...)...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("make cache-dir %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	t.Run("absolute override passes through unchanged", func(t *testing.T) {
		abs := filepath.Join(t.TempDir(), "wazero-cache")
		if got := effective(t, "TORANA_CI_CACHE="+abs); got != abs {
			t.Fatalf("effective = %q, want the absolute override %q", got, abs)
		}
	})
	t.Run("relative override is anchored at the repo root", func(t *testing.T) {
		got := effective(t, "TORANA_CI_CACHE=relative/cache")
		want := filepath.Join(repo, "relative", "cache")
		if got != want {
			t.Fatalf("effective = %q, want the anchored %q", got, want)
		}
	})
	t.Run("default is the ignored repo-local dir", func(t *testing.T) {
		want := filepath.Join(repo, ".cache", "wazero")
		if got := effective(t); got != want {
			t.Fatalf("effective = %q, want %q", got, want)
		}
	})
}

// TestMapperMultiPackage — the mapper accepts several package paths in one
// invocation (the remainder-shard query shape): the union is printed, and a
// failure in ANY package after earlier successes produces a non-zero exit
// with NO partial output.
func TestMapperMultiPackage(t *testing.T) {
	repo := repoRoot(t)
	wrapper := filepath.Join(repo, "scripts", "fixtures-for-pkg.sh")
	run := func(t *testing.T, pkgs ...string) (string, error) {
		t.Helper()
		cmd := exec.Command("sh", wrapper)
		cmd.Args = append(cmd.Args, pkgs...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// Union over packages that individually reference nothing: still fine.
	out, err := run(t, "internal/secret", "internal/metrics")
	if err != nil {
		t.Fatalf("multi-package mapper failed: %v\n%s", err, out)
	}
	if len(strings.Fields(out)) != 0 {
		t.Fatalf("expected an empty union, got:\n%s", out)
	}

	// One failing package AFTER an earlier successful walk: non-zero, and no
	// partial authoritative output from the successful one.
	out, err = run(t, "internal/metrics", "/nonexistent/pkg")
	if err == nil {
		t.Fatalf("mapper succeeded with a nonexistent package:\n%s", out)
	}
	if strings.Contains(out, "examples/plugins/") || strings.Contains(out, "test-") {
		t.Fatalf("mapper emitted partial output despite the failure:\n%s", out)
	}
}

// TestMain pins the hermetic property: no test in this package may leave the
// tracked checkout modified (fixtures, stamps, and the local cache are
// gitignored and therefore invisible to git status).
func TestMain(m *testing.M) {
	repo := ""
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		repo = strings.TrimSpace(string(out))
	}
	var before string
	if repo != "" {
		out, err := exec.Command("git", "-C", repo, "status", "--porcelain").Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "fixturebuild: git status failed before tests: %v\n", err)
			os.Exit(1)
		}
		before = string(out)
	}
	code := m.Run()
	if repo != "" {
		out, err := exec.Command("git", "-C", repo, "status", "--porcelain").Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "fixturebuild: git status failed after tests: %v\n", err)
			os.Exit(1)
		}
		if string(out) != before {
			fmt.Fprintf(os.Stderr, "fixturebuild tests modified the tracked checkout:\n%s", out)
			code = 1
		}
	}
	os.Exit(code)
}
