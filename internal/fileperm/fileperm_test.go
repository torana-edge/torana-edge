package fileperm_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/torana-edge/torana-edge/internal/fileperm"
)

// These pin the invariant itself, in whatever terms the running platform
// states it. They are deliberately free of mode-bit literals: a test that
// asserts 0600 passes on Unix and is meaningless on Windows, which is exactly
// how the audit log and the credential key came to be unprotected there.

func TestWriteNewProducesAnOwnerOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	want := []byte("0123456789abcdef0123456789abcdef")
	if err := fileperm.WriteNew(path, want); err != nil {
		t.Fatalf("WriteNew: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("content = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := fileperm.Verify(path, info); err != nil {
		t.Errorf("a file WriteNew just created is not owner-only: %v", err)
	}
}

func TestWriteNewRefusesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	if err := fileperm.WriteNew(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := fileperm.WriteNew(path, []byte("second")); err == nil {
		t.Fatal("WriteNew overwrote an existing file; the caller can no longer tell " +
			"whether it created the key or clobbered one already in use")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first" {
		t.Fatalf("content = %q after the refused write, want %q", got, "first")
	}
}

// A file created the ordinary way is widely accessible on both platforms:
// 0644 on Unix, and on Windows a file created with no explicit descriptor
// inherits its directory's, which is not the protected owner-only ACL.
func TestVerifyRejectsAWidelyAccessibleFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "open.jsonl")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := fileperm.Verify(path, info); err == nil {
		t.Fatal("Verify accepted a widely accessible file")
	}
}

func TestRestrictMakesAnExistingFileOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "open.jsonl")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fileperm.Restrict(path); err != nil {
		t.Fatalf("Restrict: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := fileperm.Verify(path, info); err != nil {
		t.Errorf("Restrict did not make the file owner-only: %v", err)
	}
}

func TestRestrictDirMakesADirectoryOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fileperm.RestrictDir(dir); err != nil {
		t.Fatalf("RestrictDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := fileperm.Verify(dir, info); err != nil {
		t.Errorf("RestrictDir did not make the directory owner-only: %v", err)
	}
	// Still usable: the point is to exclude others, not ourselves.
	if err := os.WriteFile(filepath.Join(dir, "child"), []byte("x"), 0o600); err != nil {
		t.Errorf("owner cannot write inside its own restricted directory: %v", err)
	}
}

func TestOpenAppendCreatesOwnerOnlyAndRefusesAWidenedFile(t *testing.T) {
	dir := t.TempDir()

	created := filepath.Join(dir, "audit.jsonl")
	f, err := fileperm.OpenAppend(created)
	if err != nil {
		t.Fatalf("OpenAppend on a new path: %v", err)
	}
	if _, err := f.WriteString("one\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(created)
	if err != nil {
		t.Fatal(err)
	}
	if err := fileperm.Verify(created, info); err != nil {
		t.Errorf("OpenAppend created a file that is not owner-only: %v", err)
	}

	// Re-opening our own file appends rather than truncating.
	f, err = fileperm.OpenAppend(created)
	if err != nil {
		t.Fatalf("OpenAppend on an owner-only file: %v", err)
	}
	if _, err := f.WriteString("two\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(created); err != nil {
		t.Fatal(err)
	} else if string(b) != "one\ntwo\n" {
		t.Fatalf("content = %q, want %q", b, "one\ntwo\n")
	}

	// A pre-existing widely accessible file is refused, not silently tightened
	// and not silently used.
	widened := filepath.Join(dir, "widened.jsonl")
	if err := os.WriteFile(widened, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := fileperm.OpenAppend(widened)
	if err == nil {
		_ = g.Close()
		t.Fatal("OpenAppend accepted a widely accessible existing file")
	}
}

// The security decision must describe the object that will actually be read
// or written. Verify resolves a NAME and is a pre-check only; VerifyFile
// resolves the open HANDLE. Rebinding the name after the open is what
// separates them: a path-based check would answer for the replacement while
// the writes still go to the original.
func TestVerifyFileAnswersForTheHandleNotTheName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	f, err := fileperm.OpenAppend(path)
	if err != nil {
		t.Fatalf("OpenAppend: %v", err)
	}
	defer func() { _ = f.Close() }()
	if err := fileperm.VerifyFile(f); err != nil {
		t.Fatalf("a file OpenAppend just created is not owner-only: %v", err)
	}

	// Point the name at a widely accessible file, leaving the handle where it
	// was. On Unix this is a rename over the path; the descriptor still refers
	// to the original inode.
	other := filepath.Join(dir, "other.jsonl")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, path); err != nil {
		t.Skipf("cannot rebind the name on this platform: %v", err)
	}

	// The name now resolves to something unsafe...
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := fileperm.Verify(path, info); err == nil {
		t.Error("Verify accepted the widely accessible file the name now resolves to")
	}
	// ...while the handle still refers to the file that was checked, and
	// VerifyFile must keep answering for THAT one.
	if err := fileperm.VerifyFile(f); err != nil {
		t.Errorf("VerifyFile followed the name instead of the handle: %v", err)
	}
}

// The mirror image: a handle onto a widely accessible file must be refused
// however the name is later made to look.
func TestVerifyFileRefusesAWidelyAccessibleHandle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "open.jsonl")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := fileperm.VerifyFile(f); err == nil {
		t.Fatal("VerifyFile accepted a handle onto a widely accessible file")
	}
}
