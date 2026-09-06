//go:build !windows

package pluginfiles

import (
	"fmt"
	"io/fs"
	"syscall"
)

// hardLinkCount reports how many directory entries point at this file.
//
// regularSingleLink is a security boundary: a plugin-private file with more
// than one link is a link an operator did not create, and writing through it
// would escape the plugin's own directory. So a count that cannot be obtained
// is an error, never a pass — the caller refuses rather than proceeding on an
// unknown.
func hardLinkCount(_ string, info fs.FileInfo) (uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("plugin file link count is unavailable on this platform")
	}
	return uint64(stat.Nlink), nil
}
