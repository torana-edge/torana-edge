package plugincmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every way Torana compiles a plugin must pass -trimpath, or approval by digest
// does not mean anything.
//
// Without it the absolute build path is baked into the binary. `torana plugin
// install` stages the source into os.MkdirTemp("", "torana-build-*") — a
// randomized directory — so two installs of byte-identical source produce
// different digests, and the operator's approval is invalidated by reinstalling
// the same thing. Reproduced before this was fixed:
//
//	ec280dbe…  build 1
//	aaf7c849…  build 2   (same source, different temp dir)
//
// and with -trimpath both builds produce e375981c…
//
// This is a source check rather than a build, so it runs everywhere without a
// toolchain or network. The behavioural guard — build twice, compare digests —
// lives in torana-plugins CI, where real bundles are built.
func TestEveryPluginBuildUsesTrimpath(t *testing.T) {
	for _, rel := range []string{"plugincmd.go", "install.go"} {
		raw, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, `"go", "build"`) {
				continue
			}
			if !strings.Contains(line, "-trimpath") {
				t.Errorf("%s:%d compiles a plugin without -trimpath, so its digest "+
					"depends on the build directory:\n\t%s", rel, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// The Makefile builds the WASM test fixtures. They are not approved bundles, but
// a fixture whose digest moves between builds makes approval-migration and
// digest-binding tests non-deterministic.
func TestMakefileWASMBuildUsesTrimpath(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	for i, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "WASM_BUILD") {
			continue
		}
		if !strings.Contains(line, "-trimpath") {
			t.Errorf("Makefile:%d builds WASM without -trimpath:\n\t%s", i+1, strings.TrimSpace(line))
		}
		return
	}
	t.Error("Makefile has no WASM_BUILD definition — has the build moved?")
}
