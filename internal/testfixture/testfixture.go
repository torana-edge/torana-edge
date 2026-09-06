// Package testfixture gates tests on the built WASM plugin fixtures.
//
// The fixtures are build artifacts (`*.wasm` is gitignored), so a plain
// `go test ./...` finds none of them. Every plugin-behaviour test then skips
// and each package still reports `ok` — a green run that covered none of the
// sandbox, the hook pipeline, or the capability boundary. That is the most
// misleading state this repository can be in, so it is announced rather than
// left to be inferred from `-v` output nobody passes.
//
// `make test` sets TORANA_E2E=1, which turns a missing fixture from a skip into
// a failure. That is the real gate; this package exists to make the difference
// visible when someone runs the other thing.
package testfixture

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Require skips (or, under TORANA_E2E, fails) when a fixture is not built.
//
// It takes testing.TB rather than *testing.T so benchmarks build their pipeline
// through exactly the same path the tests do — a benchmark measuring a
// differently-constructed pipeline would not be measuring what ships.
func Require(t testing.TB, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		if os.Getenv("TORANA_E2E") != "" {
			// Explicit return rather than relying on Fatalf's runtime.Goexit:
			// the two branches are mutually exclusive outcomes, and saying so
			// keeps the helper correct under any TB implementation.
			t.Fatalf("%s missing — run 'make testdata' (err: %v)", path, err)
			return
		}
		t.Skipf("%s not built — run 'make testdata'", path)
	}
}

var warnOnce sync.Once

// WarnIfUnbuilt prints one unmissable banner when fixtures are absent and the
// strict gate is off. Call it from TestMain, before m.Run.
//
// dir is the fixture root relative to the calling package, e.g.
// "../../examples/plugins".
func WarnIfUnbuilt(dir string) {
	warnOnce.Do(func() {
		warnIfUnbuilt(os.Stderr, dir, os.Getenv("TORANA_E2E"))
	})
}

// warnIfUnbuilt is the testable body: it reports whether it warned, so the
// package's own test can prove the banner fires on an empty tree and stays
// quiet on a built one — without a test having to delete real fixtures.
func warnIfUnbuilt(w io.Writer, dir, e2e string) bool {
	if e2e != "" {
		return false // the strict gate will fail on the first missing fixture
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*", "plugin.wasm"))
	if err != nil || len(matches) > 0 {
		return false
	}
	fmt.Fprint(w, "\n"+
		"┌──────────────────────────────────────────────────────────────────┐\n"+
		"│  WASM fixtures are NOT built.                                    │\n"+
		"│                                                                  │\n"+
		"│  Every plugin-behaviour test will SKIP and this package will     │\n"+
		"│  still report `ok`. This run does not cover the plugin sandbox,  │\n"+
		"│  the hook pipeline, or the capability boundary.                  │\n"+
		"│                                                                  │\n"+
		"│  Run `make test` — that is the gate CI runs.                     │\n"+
		"└──────────────────────────────────────────────────────────────────┘\n\n")
	return true
}
