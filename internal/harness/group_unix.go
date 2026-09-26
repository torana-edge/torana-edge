//go:build linux || darwin

package harness

import (
	"os"
	"syscall"
)

func preserveGroup(file *os.File, path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return file.Chown(-1, int(stat.Gid))
	}
	return nil
}
