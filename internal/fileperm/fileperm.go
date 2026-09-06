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
)

// Verify returns nil when path is accessible only by its owner. info must
// describe path; it is used on platforms where mode bits carry the answer, and
// ignored where the access-control list does.
func Verify(path string, info fs.FileInfo) error {
	return verify(path, info)
}

// Restrict makes a FILE at path accessible only by its owner.
func Restrict(path string) error {
	return restrict(path, false)
}

// RestrictDir makes a DIRECTORY at path accessible only by its owner, and
// makes that the default for entries created inside it. Verify applies to a
// directory unchanged — the rule about who may reach it is the same rule.
func RestrictDir(path string) error {
	return restrict(path, true)
}

// WriteNew creates path with owner-only access and writes data to it, failing
// if path already exists.
//
// The order matters: create, restrict, then write. The file is empty for the
// moment it exists with whatever access it inherited, so no secret is ever
// readable through the gap.
func WriteNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := restrict(path, false); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

// OpenAppend opens path for appending. A file this call creates is made
// owner-only; a file that already exists is REFUSED unless it already is,
// because widening or narrowing an operator's file without saying so is how a
// confidentiality boundary goes missing quietly.
func OpenAppend(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err == nil {
		if rerr := restrict(path, false); rerr != nil {
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
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s must name a regular file", path)
	}
	if err := verify(path, info); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
