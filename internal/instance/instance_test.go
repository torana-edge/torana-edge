package instance

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/torana-edge/torana-edge/internal/fileperm"
)

func TestLifetimeLockAndReadOnlyProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uncreated", "instance.lock")
	if active, err := Running(path); err != nil || active {
		t.Fatalf("initial probe = %v, %v", active, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("probe created directory")
	}
	owner, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	if _, err := fileperm.EnsureDir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if active, err := Running(path); err != nil || !active {
		t.Fatalf("held probe = %v, %v", active, err)
	}
	if other, err := Acquire(path); !errors.Is(err, ErrLocked) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("second owner = %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if active, err := Running(path); err != nil || active {
		t.Fatalf("released probe = %v, %v", active, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("lock file must not be unlinked", err)
	}
}

func TestRecordReplacementAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.json")
	for _, r := range []Record{{"127.0.0.1:8001", "first"}, {"[::1]:8002", "second"}} {
		if err := WriteRecord(path, r); err != nil {
			t.Fatal(err)
		}
		got, err := ReadRecord(path)
		if err != nil || got != r {
			t.Fatalf("read = %v, %v", got, err)
		}
	}
	if err := os.WriteFile(path, []byte(`{"address":"127.0.0.1:8001"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRecord(path); err == nil {
		t.Fatal("accepted incomplete record")
	}
}

func TestProbeReleasesLockBeforeImmediateAcquisition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if active, err := Running(path); err != nil || active {
			t.Fatalf("probe=%v %v", active, err)
		}
		owner, err := Acquire(path)
		if err != nil {
			t.Fatalf("probe kept the lock: %v", err)
		}
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
