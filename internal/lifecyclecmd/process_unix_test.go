//go:build !windows

package lifecyclecmd

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
)

func TestNativeConnectionRefusal(t *testing.T) {
	err := fmt.Errorf("control client: %w", &net.OpError{Op: "dial", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}})
	if !connectionRefused(err) || connectionRefused(os.ErrPermission) {
		t.Fatal("incorrect refusal classification")
	}
}
