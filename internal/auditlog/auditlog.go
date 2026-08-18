// Package auditlog implements Torana's operator-owned, bounded JSONL audit
// sink. It deliberately lives in the host: WASM guests have no filesystem,
// and granting one an arbitrary local path would weaken the plugin sandbox.
package auditlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	DefaultMaxFileBytes int64 = 16 << 20
	DefaultMaxFiles           = 5
	maxConfiguredFiles        = 100
)

// Config controls the sensitive intercepted-request audit. The feature is
// disabled by default and never chooses a path on the operator's behalf.
// Zero bounds select the documented conservative defaults.
type Config struct {
	Enabled      bool   `json:"enabled,omitempty"`
	Path         string `json:"path,omitempty"`
	MaxFileBytes int64  `json:"max_file_bytes,omitempty"`
	MaxFiles     int    `json:"max_files,omitempty"`
}

// Record is the versioned, bounded-schema description of one intercepted
// caller request. It intentionally contains no arbitrary error text or raw
// request body. Tool arguments remain private except for the explicit intent
// convention, which is itself documented as sensitive.
type Record struct {
	SchemaVersion        int        `json:"schema_version"`
	Timestamp            string     `json:"timestamp"`
	RequestID            uint64     `json:"request_id"`
	InitialProvider      string     `json:"initial_provider"`
	Provider             string     `json:"provider"`
	Format               string     `json:"format"`
	Path                 string     `json:"path"`
	InitialModel         string     `json:"initial_model,omitempty"`
	Model                string     `json:"model,omitempty"`
	Status               int        `json:"status"`
	IngressBytes         int64      `json:"ingress_bytes"`
	UpstreamRequestBytes int64      `json:"upstream_request_bytes"`
	Plugins              []string   `json:"plugins,omitempty"`
	Verdict              string     `json:"verdict,omitempty"`
	VerdictPlugin        string     `json:"verdict_plugin,omitempty"`
	PluginFailure        bool       `json:"plugin_failure,omitempty"`
	ErrorCode            string     `json:"error_code,omitempty"`
	ToolCalls            []ToolCall `json:"tool_calls,omitempty"`
}

type ToolCall struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Intent string `json:"intent,omitempty"`
}

func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Path) == "" {
		return errors.New("audit.path must be set when audit is enabled")
	}
	if !filepath.IsAbs(c.Path) {
		return errors.New("audit.path must be absolute")
	}
	if c.MaxFileBytes < 0 {
		return errors.New("audit.max_file_bytes must not be negative")
	}
	if c.MaxFiles < 0 || c.MaxFiles > maxConfiguredFiles {
		return fmt.Errorf("audit.max_files must be between 0 and %d", maxConfiguredFiles)
	}
	return nil
}

func (c Config) maxFileBytes() int64 {
	if c.MaxFileBytes == 0 {
		return DefaultMaxFileBytes
	}
	return c.MaxFileBytes
}

func (c Config) maxFiles() int {
	if c.MaxFiles == 0 {
		return DefaultMaxFiles
	}
	return c.MaxFiles
}

// Writer serializes complete JSONL records and rotates them under one lock.
// It does not claim fsync durability: Close flushes the file descriptor, while
// per-request Sync would turn an observability feature into a data-plane disk
// latency dependency.
type Writer struct {
	mu       sync.Mutex
	config   Config
	file     *os.File
	size     int64
	closed   bool
	disabled bool
	failed   error
}

// Open validates the destination before returning a writer. Disabled configs
// never touch the filesystem.
func Open(config Config) (*Writer, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	w := &Writer{config: config, disabled: !config.Enabled}
	if w.disabled {
		return w, nil
	}
	if err := validateExistingTarget(config.Path); err != nil {
		return nil, err
	}
	if err := w.openActive(); err != nil {
		return nil, err
	}
	return w, nil
}

func validateExistingTarget(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect audit path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("audit.path must not be a symbolic link")
	}
	if !info.Mode().IsRegular() {
		return errors.New("audit.path must name a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("audit.path must not be accessible by group or others")
	}
	return nil
}

func (w *Writer) openActive() error {
	f, err := os.OpenFile(w.config.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open audit path: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat audit path: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		_ = f.Close()
		return errors.New("audit.path must remain an owner-only regular file")
	}
	w.file = f
	w.size = info.Size()
	return nil
}

// Append writes one compact JSON value and a newline. Calls are serialized, so
// concurrent requests can never interleave records. Disabled writers are a
// no-op and do not marshal the record.
func (w *Writer) Append(record any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("audit writer is closed")
	}
	if w.disabled {
		return nil
	}
	if w.failed != nil {
		return w.failed
	}
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode audit record: %w", err)
	}
	line = append(line, '\n')
	if w.size > 0 && w.size+int64(len(line)) > w.config.maxFileBytes() {
		if err := w.rotate(); err != nil {
			w.failed = err
			return w.failed
		}
	}
	before := w.size
	n, err := w.file.Write(line)
	if err != nil || n != len(line) {
		// Best effort restores the last complete-line boundary. If truncate also
		// fails, return both errors rather than hiding possible corruption.
		writeErr := err
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		if truncateErr := w.file.Truncate(before); truncateErr != nil {
			return fmt.Errorf("append audit record: %w; restore boundary: %v", writeErr, truncateErr)
		}
		return fmt.Errorf("append audit record: %w", writeErr)
	}
	w.size += int64(n)
	return nil
}

func (w *Writer) rotate() error {
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("close audit file for rotation: %w", err)
	}
	w.file = nil
	maxFiles := w.config.maxFiles()
	oldest := fmt.Sprintf("%s.%d", w.config.Path, maxFiles-1)
	if err := os.Remove(oldest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove oldest audit file: %w", err)
	}
	for i := maxFiles - 2; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", w.config.Path, i)
		to := fmt.Sprintf("%s.%d", w.config.Path, i+1)
		if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rotate audit file %d: %w", i, err)
		}
	}
	if maxFiles > 1 {
		if err := os.Rename(w.config.Path, w.config.Path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rotate active audit file: %w", err)
		}
	} else if err := os.Remove(w.config.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove active audit file: %w", err)
	}
	if err := w.openActive(); err != nil {
		return err
	}
	return nil
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return nil
	}
	return w.file.Close()
}
