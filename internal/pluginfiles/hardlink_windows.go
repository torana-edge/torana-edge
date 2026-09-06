//go:build windows

package pluginfiles

import "io/fs"

// hardLinkCount has no Windows implementation: os.FileInfo.Sys() returns a
// syscall.Win32FileAttributeData, which carries no link count, and obtaining
// one needs an open handle plus GetFileInformationByHandle. Reporting "not
// known" makes the caller skip the check rather than silently treat every
// file as unlinked.
//
// The other protections in regularSingleLink — regular-file and symlink
// checks — still apply on Windows.
func hardLinkCount(fs.FileInfo) (links uint64, known bool) { return 0, false }
