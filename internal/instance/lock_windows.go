//go:build windows

package instance

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

// Explicit release avoids Windows' potentially delayed cleanup on CloseHandle.
func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}

func lockBusy(err error) bool { return errors.Is(err, windows.ERROR_LOCK_VIOLATION) }
