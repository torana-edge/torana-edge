package pluginfiles

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/torana-edge/torana-edge/internal/wasm"
)

const maxHostCallPayload = 64 << 10

type Store struct {
	root string
	mu   sync.Mutex
}

func New(root string) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("plugin file root is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

func pluginDir(root, plugin string) string {
	sum := sha256.Sum256([]byte(plugin))
	return filepath.Join(root, hex.EncodeToString(sum[:16]))
}

func (s *Store) target(plugin, logical string) (string, error) {
	if logical == "" || filepath.IsAbs(logical) || strings.Contains(logical, "\\") {
		return "", fmt.Errorf("unsafe logical path")
	}
	for _, part := range strings.Split(logical, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("unsafe logical path")
		}
	}
	base := pluginDir(s.root, plugin)
	target := filepath.Join(base, filepath.FromSlash(logical))
	if target != base && !strings.HasPrefix(target, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes plugin directory")
	}
	return target, nil
}

func secureParent(path, base string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	for current := filepath.Dir(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("unsafe plugin file directory")
		}
		if err := os.Chmod(current, 0o700); err != nil {
			return err
		}
		if current == base {
			break
		}
		parent := filepath.Dir(current)
		if parent == current || !strings.HasPrefix(current, base+string(os.PathSeparator)) {
			return fmt.Errorf("path escapes plugin directory")
		}
	}
	return nil
}

func regularSingleLink(path string, allowMissing bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && allowMissing {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("plugin file is not a regular file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
		return fmt.Errorf("plugin file has multiple hard links")
	}
	return nil
}

func (s *Store) Append(plugin, logical string, data []byte, resource wasm.FileResource) error {
	if len(data) > maxHostCallPayload {
		return fmt.Errorf("append exceeds 64 KiB host-call limit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.target(plugin, logical)
	if err != nil {
		return err
	}
	base := pluginDir(s.root, plugin)
	if err := secureParent(path, base); err != nil {
		return err
	}
	if err := regularSingleLink(path, true); err != nil {
		return err
	}
	if resource.MaxBytes <= 0 {
		return fmt.Errorf("file has no size budget")
	}
	if int64(len(data)) > resource.MaxBytes {
		return fmt.Errorf("append exceeds approved file size")
	}
	var size int64
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if size+int64(len(data)) > resource.MaxBytes {
		if err := rotate(path, resource.RetainedFiles); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := regularSingleLink(path, false); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

func rotate(path string, retained int) error {
	if retained <= 0 {
		return os.Remove(path)
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", path, retained))
	for i := retained - 1; i >= 1; i-- {
		old := fmt.Sprintf("%s.%d", path, i)
		next := fmt.Sprintf("%s.%d", path, i+1)
		if err := os.Rename(old, next); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(path, path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) Read(plugin, logical string, resource wasm.FileResource) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.target(plugin, logical)
	if err != nil {
		return nil, err
	}
	if err := regularSingleLink(path, false); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	limit := resource.MaxBytes
	if limit <= 0 || limit > 64<<20 {
		limit = 64 << 20
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds read limit")
	}
	return data, nil
}

func (s *Store) Write(plugin, logical string, data []byte, resource wasm.FileResource) error {
	if resource.MaxBytes <= 0 || len(data) > maxHostCallPayload || int64(len(data)) > resource.MaxBytes {
		return fmt.Errorf("write exceeds approved limit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.target(plugin, logical)
	if err != nil {
		return err
	}
	base := pluginDir(s.root, plugin)
	if err := secureParent(path, base); err != nil {
		return err
	}
	if err := regularSingleLink(path, true); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".plugin-file-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (s *Store) Delete(plugin, logical string, _ wasm.FileResource) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.target(plugin, logical)
	if err != nil {
		return err
	}
	if err := regularSingleLink(path, true); err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Store) List(plugin, prefix string, resources map[string]wasm.FileResource) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for logical := range resources {
		if !strings.HasPrefix(logical, prefix) {
			continue
		}
		path, err := s.target(plugin, logical)
		if err != nil {
			return nil, err
		}
		if err := regularSingleLink(path, false); err == nil {
			out = append(out, logical)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

// OperatorList exposes only logical names for CLI inspection; OS paths remain
// an implementation detail even to operators.
func (s *Store) OperatorList(plugin string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := pluginDir(s.root, plugin)
	var out []string
	err := filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if path == base {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe plugin file entry")
		}
		if entry.IsDir() {
			return nil
		}
		if err := regularSingleLink(path, false); err != nil {
			return err
		}
		relative, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(relative))
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	sort.Strings(out)
	return out, err
}

func (s *Store) OperatorRead(plugin, logical string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.target(plugin, logical)
	if err != nil {
		return nil, err
	}
	if err := regularSingleLink(path, false); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func (s *Store) OperatorPurge(plugin string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := pluginDir(s.root, plugin)
	info, err := os.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe plugin directory")
	}
	return os.RemoveAll(base)
}
