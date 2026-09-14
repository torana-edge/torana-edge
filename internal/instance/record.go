package instance

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Record is a routing hint, not authority to terminate a PID. Callers verify
// service identity through the loopback API before taking any action.
type Record struct {
	Address    string `json:"address"`
	InstanceID string `json:"instance_id"`
}

func ReadRecord(path string) (Record, error) {
	var r Record
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return r, err
	}
	if len(raw) > 4096 {
		return r, fmt.Errorf("instance record is too large")
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	if r.InstanceID == "" || r.Address == "" {
		return r, fmt.Errorf("incomplete instance record")
	}
	return r, nil
}

func WriteRecord(path string, r Record) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".instance-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if _, err := f.Write(raw); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
