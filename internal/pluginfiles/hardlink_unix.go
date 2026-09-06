//go:build !windows

package pluginfiles

import (
	"io/fs"
	"syscall"
)

// hardLinkCount reports the number of directory entries pointing at this
// inode. A plugin-private file with more than one is a link an operator did
// not create, so the caller refuses to write through it.
//
// known is false when the platform does not expose a link count; the caller
// then skips the check rather than inventing a value.
func hardLinkCount(info fs.FileInfo) (links uint64, known bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Nlink), true
}
