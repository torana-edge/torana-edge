package testfixture

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The banner exists because an unbuilt tree otherwise reports `ok` while
// covering none of the plugin sandbox. These prove it fires exactly when it
// should, so the guarantee does not quietly rot the way the coverage it
// announces once did.
func TestWarnIfUnbuilt(t *testing.T) {
	built := t.TempDir()
	if err := os.MkdirAll(filepath.Join(built, "test-observer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(built, "test-observer", "plugin.wasm"), []byte("\x00asm"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		dir      string
		e2e      string
		wantWarn bool
	}{
		{name: "unbuilt tree warns", dir: t.TempDir(), wantWarn: true},
		{name: "built tree stays quiet", dir: built, wantWarn: false},
		{name: "strict gate stays quiet, it fails instead", dir: t.TempDir(), e2e: "1", wantWarn: false},
		{name: "missing directory warns", dir: filepath.Join(t.TempDir(), "absent"), wantWarn: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			got := warnIfUnbuilt(&buf, tt.dir, tt.e2e)
			if got != tt.wantWarn {
				t.Fatalf("warned = %v, want %v", got, tt.wantWarn)
			}
			if got && !strings.Contains(buf.String(), "make test") {
				t.Errorf("banner must name the command that fixes it, got:\n%s", buf.String())
			}
			if !got && buf.Len() != 0 {
				t.Errorf("expected no output, got:\n%s", buf.String())
			}
		})
	}
}

// Require is the shared gate the three fixture-dependent packages delegate to.
// Under TORANA_E2E a missing fixture must FAIL rather than skip — that is the
// whole difference between `make test` and a bare `go test ./...`.
func TestRequireIsStrictUnderE2E(t *testing.T) {
	t.Setenv("TORANA_E2E", "1")
	fake := &recordingTB{TB: t}
	Require(fake, filepath.Join(t.TempDir(), "absent", "plugin.wasm"))
	if !fake.failed {
		t.Error("Require must fail on a missing fixture when TORANA_E2E is set")
	}
	if fake.skipped {
		t.Error("Require must not skip when TORANA_E2E is set")
	}
}

func TestRequireSkipsWithoutE2E(t *testing.T) {
	t.Setenv("TORANA_E2E", "")
	fake := &recordingTB{TB: t}
	Require(fake, filepath.Join(t.TempDir(), "absent", "plugin.wasm"))
	if !fake.skipped {
		t.Error("Require must skip on a missing fixture when TORANA_E2E is unset")
	}
	if fake.failed {
		t.Error("Require must not fail when TORANA_E2E is unset")
	}
}

// recordingTB captures which terminal path Require took. Fatalf/Skipf must not
// actually stop this goroutine, so neither is forwarded to the real TB.
type recordingTB struct {
	testing.TB
	failed  bool
	skipped bool
}

func (r *recordingTB) Helper()                           {}
func (r *recordingTB) Fatalf(string, ...any)             { r.failed = true }
func (r *recordingTB) Skipf(string, ...any)              { r.skipped = true }
func (r *recordingTB) Errorf(format string, args ...any) { r.failed = true }
