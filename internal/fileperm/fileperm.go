// Package fileperm enforces one invariant on the files Torana treats as
// confidential — the credential-encryption key and the audit trail — on every
// platform it ships: only the owner may read them.
//
// The invariant needs a platform-specific SPELLING, not a platform-specific
// STANDARD. Unix expresses it as mode bits; Windows expresses it as a DACL,
// where os.FileMode is not merely different but actively misleading: Perm()
// reports 0666 for any ordinary readable file whatever its ACL says, and
// os.OpenFile's perm argument controls nothing but the read-only attribute.
// Checking mode bits there rejects every path, and creating a file "0600"
// there protects nothing — which is why this package exists rather than an
// `if runtime.GOOS == "windows"` that skips the check.
package fileperm

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Verify returns nil when path is accessible only by its owner. info must
// describe path; it is used on platforms where mode bits carry the answer, and
// ignored where the access-control list does.
//
// This resolves the file by NAME, so it is a PRE-CHECK: a name can be made to
// resolve to a different object between this call and a later open. Anything
// that goes on to read or write the file must decide on the open handle
// instead — see VerifyFile.
func Verify(path string, info fs.FileInfo) error {
	return verify(path, info)
}

// VerifyFile returns nil when the object behind an OPEN handle is accessible
// only by its owner. The security decision then describes the same object that
// will be read or written, which a path-based check cannot promise.
func VerifyFile(f *os.File) error {
	return verifyFile(f)
}

// Restrict makes a FILE at path accessible only by its owner.
func Restrict(path string) error {
	return restrict(path, false)
}

// RestrictDir makes a DIRECTORY at path accessible only by its owner, and
// makes that the default for entries created inside it. Verify applies to a
// directory unchanged — the rule about who may reach it is the same rule.
//
// Prefer EnsureDir at a startup path. Applying this unconditionally is what
// breaks concurrent callers; see there.
func RestrictDir(path string) error {
	return restrict(path, true)
}

// EnsureDir makes a directory owner-only if it is not already, and reports an
// error when it cannot be made so.
//
// The "if it is not already" is the point, not an optimization. On Windows,
// applying a protected DACL to a container starts a propagation pass over its
// children, and a pass that lands between another process's restrict of a file
// and that process's verify of it overwrites a child which was already
// correct. Startup paths call this on every run, so re-asserting a state that
// already holds is not free — it is precisely what breaks the neighbours. It
// surfaced as a first-run flake when several processes opened the same data
// directory at once:
//
//	failed to create secret key file: …\.torana-new-2613133976 inherits
//	access from its parent directory; it must carry a protected owner-only ACL
//
// changed reports whether anything was written, so the no-rewrite property can
// be asserted directly rather than inferred from a timestamp.
func EnsureDir(path string) (changed bool, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if verify(path, info) == nil {
		return false, nil
	}
	if err := restrict(path, true); err != nil {
		return true, err
	}
	info, err = os.Stat(path)
	if err != nil {
		return true, err
	}
	return true, verify(path, info)
}

// Secure applies owner-only access to a file this process is creating and
// CONFIRMS it on the open handle. Every confidential path goes through this
// one function, so "protected" means the same thing for the credential key,
// the audit trail and the CA private key.
//
// One apply and one check, not a retry loop: retrying was an admission that
// something else could still be writing over the file, and a bounded retry
// cannot fix that — a competing write can land after the last check just as
// easily as before it. The competing writer was an inheritable directory ACE
// propagating over its children; that is gone (see the Windows restrict), so
// there is nothing left to lose a race against.
//
// The check is on the HANDLE, so it describes the object that will actually be
// written, not whatever the name resolves to by then.
func Secure(path string, f *os.File) error {
	if err := restrict(path, false); err != nil {
		return err
	}
	return verifyFile(f)
}

// WriteNew publishes path with owner-only access holding data, failing if path
// already exists.
//
// The destination name must never exist in an INCOMPLETE state. Creating it
// with O_EXCL and filling it afterwards left an empty file at the final name
// for the duration of the write, so a concurrent process that lost the race
// saw the name, read nothing, and failed on a zero-length key — while the
// promise was that it would read the key that won.
//
// So the file is built under a temporary name and published with a link, which
// fails when the destination exists. The link decides the winner atomically,
// and the file a loser then opens is already complete, restricted and synced.
// (Hard links are the one portable exclusive-publish primitive: os.Rename
// replaces silently on both platforms, which is the opposite of what is
// wanted here.)
func WriteNew(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".torana-new-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Removes the temporary name on every path: the failure ones, and the
	// success one where it is now a second link to the published file.
	defer func() { _ = os.Remove(tmpName) }()

	if err := Secure(tmpName, tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// fs.ErrExist here means someone else published first; the caller reads
	// theirs. Anything else is a real failure.
	return os.Link(tmpName, path)
}

// OpenAppend opens path for appending. A file this call creates is made
// owner-only; a file that already exists is REFUSED unless it already is,
// because widening or narrowing an operator's file without saying so is how a
// confidentiality boundary goes missing quietly.
func OpenAppend(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err == nil {
		if rerr := Secure(path, f); rerr != nil {
			_ = f.Close()
			return nil, rerr
		}
		return f, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	f, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	// Everything from here decides on the HANDLE. Opening the path and then
	// asking about the PATH is the substitution this guards against: the
	// answer would describe whatever the name resolves to now, while the
	// writes go to whatever it resolved to a moment ago.
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s must name a regular file", path)
	}
	if err := verifyFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
