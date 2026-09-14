package installertest

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestInstallerWindowsLockedExecutable(t *testing.T) {
	f := newInstaller(t, "windows", runtime.GOARCH)
	destination := filepath.Join(f.installDir, f.bin)
	must(t, os.WriteFile(destination, []byte("previous version"), 0o700))
	path, err := syscall.UTF16PtrFromString(destination)
	must(t, err)
	// Disallow write/delete sharing, as an executing Windows binary does.
	handle, err := syscall.CreateFile(path, syscall.GENERIC_READ, syscall.FILE_SHARE_READ,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	must(t, err)
	t.Cleanup(func() { must(t, syscall.CloseHandle(handle)) })
	output, err := f.run()
	if err == nil || !strings.Contains(output, "torana installer:") {
		t.Fatalf("locked destination replacement did not fail: %v\n%s", err, output)
	}
	data, err := os.ReadFile(destination)
	must(t, err)
	if string(data) != "previous version" {
		t.Fatal("failed replacement changed the locked executable")
	}
}
