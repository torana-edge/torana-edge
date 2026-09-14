package lifecyclecmd

import (
	"errors"
	"golang.org/x/sys/windows"
	"os/exec"
	"syscall"
)

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
}

func connectionRefused(err error) bool { return errors.Is(err, windows.WSAECONNREFUSED) }
