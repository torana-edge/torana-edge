//go:build !windows

package lifecyclecmd

import (
	"errors"
	"os/exec"
	"syscall"
)

func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

func connectionRefused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }
