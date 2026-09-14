package lifecyclecmd

import (
	"fmt"
	"golang.org/x/sys/windows"
	"net"
	"os"
	"testing"
)

func TestNativeConnectionRefusal(t *testing.T) {
	err := fmt.Errorf("control client: %w", &net.OpError{Op: "dial", Err: &os.SyscallError{Syscall: "connectex", Err: windows.WSAECONNREFUSED}})
	if !connectionRefused(err) || connectionRefused(os.ErrPermission) {
		t.Fatal("incorrect Winsock refusal classification")
	}
}
