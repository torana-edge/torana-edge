//go:build windows

package pluginfiles

import (
	"fmt"
	"io/fs"
	"syscall"
)

// hardLinkCount reports how many directory entries point at this file.
//
// os.FileInfo.Sys() on Windows returns a Win32FileAttributeData, which carries
// no link count, so the count has to come from an open handle:
// GetFileInformationByHandle fills NumberOfLinks.
//
// This must not report "unknown". regularSingleLink is a security boundary —
// it stops a plugin file operation from writing through a link an operator did
// not create — and a platform that answered "cannot tell" would silently
// weaken that boundary on Windows while Unix kept enforcing it. An error here
// is therefore returned, and the caller refuses the operation.
func hardLinkCount(path string, _ fs.FileInfo) (uint64, error) {
	namep, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("plugin file path is not representable: %w", err)
	}
	// FILE_FLAG_BACKUP_SEMANTICS is required to open a directory handle, and
	// harmless for a regular file. No sharing restrictions beyond the defaults:
	// this only reads metadata.
	handle, err := syscall.CreateFile(
		namep,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return 0, fmt.Errorf("open plugin file to count links: %w", err)
	}
	defer func() { _ = syscall.CloseHandle(handle) }()

	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &info); err != nil {
		return 0, fmt.Errorf("read plugin file link count: %w", err)
	}
	return uint64(info.NumberOfLinks), nil
}
