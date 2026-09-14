// Package instance coordinates processes sharing one Torana managed store.
package instance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/torana-edge/torana-edge/internal/fileperm"
)

var ErrLocked = errors.New("another Torana process owns this managed store")

// Lock holds an OS lock until Close or process exit. The file is deliberately
// not deleted: unlinking a locked inode would allow a second owner on Unix.
type Lock struct{ file *os.File }

func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("instance lock is not a regular file: %s", path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		if lockBusy(err) {
			return nil, fmt.Errorf("%w (%s)", ErrLocked, path)
		}
		return nil, err
	}
	if err := fileperm.Secure(path, f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Lock{file: f}, nil
}

func (l *Lock) Close() error { return l.file.Close() }

// Running is read-only: it never creates the directory or lock file.
func Running(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	err = lockFile(f)
	if lockBusy(err) {
		return true, nil
	}
	return false, err
}
