//go:build !windows

package fileperm

import (
	"fmt"
	"io/fs"
	"os"
)

// On Unix the mode bits ARE the answer, so verify reads them from the
// already-obtained FileInfo rather than racing a second stat against the path.
func verify(path string, info fs.FileInfo) error {
	if info == nil {
		return fmt.Errorf("%s: no file info to check owner-only access against", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s is accessible by group or others (mode %04o); it must be owner-only (0600)", path, perm)
	}
	return nil
}

// verifyFile asks the same question of an open handle. f.Stat is fstat(2) on
// the descriptor, so the answer describes the object the descriptor refers to
// however the name has since been rebound.
func verifyFile(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", f.Name(), err)
	}
	return verify(f.Name(), info)
}

func restrict(path string, dir bool) error {
	mode := fs.FileMode(0o600)
	if dir {
		// A directory needs the execute bit to be traversable at all.
		mode = 0o700
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("restrict %s to its owner: %w", path, err)
	}
	return nil
}
