package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// No build artifacts may be tracked outside their intended locations.
//
// A 7 MB WASM binary was committed at the repository root during the v2
// migration. The cause is easy to repeat: `go build ./examples/plugins/<name>/`
// writes an executable named after the DIRECTORY, with no extension, into the
// current working directory — so `.gitignore`'s `*.wasm` rule does not match it
// and `git add -A` picks it up.
//
// A reviewer caught it. This makes the repository catch it instead: the failure
// is silent, the artifact is large, and the next person to run a one-off build
// while porting a fixture will do exactly the same thing.
func TestNoStrayBuildArtifactsAreTracked(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	tracked := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	if len(tracked) < 10 {
		t.Fatalf("git ls-files returned %d paths; this guard would assert nothing", len(tracked))
	}

	// Names a stray `go build` would produce: one per package directory under
	// examples/plugins, written into whatever directory the build ran from.
	fixtures := map[string]bool{}
	entries, err := os.ReadDir(filepath.Join("examples", "plugins"))
	if err != nil {
		t.Fatalf("read fixture dirs: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			fixtures[e.Name()] = true
		}
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixture directories found; this guard would assert nothing")
	}

	for _, path := range tracked {
		// A stray output sits outside examples/plugins/<name>/ but is named
		// after one of them.
		base := filepath.Base(path)
		if !fixtures[base] {
			continue
		}
		if filepath.Dir(path) == filepath.Join("examples", "plugins") {
			continue // the fixture directory itself
		}
		t.Errorf("%s is tracked and named after fixture %q — it looks like a stray "+
			"`go build ./examples/plugins/%s/` output. Build with an explicit -o "+
			"into the fixture directory, or use `make testdata`.", path, base, base)
	}

	// Belt and braces: nothing executable and large should be tracked at all.
	for _, path := range tracked {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Size() < 1<<20 {
			continue
		}
		if isBinary(t, path) {
			t.Errorf("%s is a tracked binary of %d bytes; build artifacts must not "+
				"be committed", path, info.Size())
		}
	}
}

// isBinary reports whether the file starts with a known executable magic
// number. Cheap and specific — matching on "contains NUL bytes" would flag
// legitimate test fixtures.
func isBinary(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := f.Read(magic[:]); err != nil {
		return false
	}
	switch {
	case magic == [4]byte{0x7f, 'E', 'L', 'F'}: // ELF
		return true
	case magic == [4]byte{0x00, 0x61, 0x73, 0x6d}: // WASM
		return true
	case magic[0] == 0xcf && magic[1] == 0xfa: // Mach-O 64
		return true
	}
	return false
}
