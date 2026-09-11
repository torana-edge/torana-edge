// Package pluginstate is durable, per-plugin key/value storage.
//
// Plugins already have two places to keep things, and neither survives a
// restart:
//
//   - env.meta_* is request-scoped. It is dropped when the request ends.
//   - env.cache_* is cross-request but TTL'd and plugin-private;
//     env.shared_cache_* is the deliberate shared flat keyspace.
//
// Nothing existed for state a plugin must still have after the proxy restarts:
// a cache-warming plugin's stored prefixes, an index's last-sync marker, a
// digest of the last configuration a plugin acted on. This is that.
//
// Two differences from both cache families are deliberate:
//
//   - Keys are namespaced per plugin, like env.meta_* and env.cache_*. Durable
//     state is never part of the explicit shared-cache exchange channel.
//   - Entries do not expire. Expiry is the plugin's business — the host cannot
//     know whether a stored prefix is still wanted.
//
// # On disk
//
// State is written to a single JSON file, replaced atomically. This suits the
// expected shape — tens to hundreds of keys, written occasionally, read on
// startup — and keeps the whole store recoverable by hand with a text editor.
//
// It is explicitly not built for high write rates, and the cost is worth
// knowing before you design around it. EVERY Set re-marshals the whole store,
// writes a temp file, fsyncs it, renames it, and fsyncs the directory. So the
// cost of one write scales with the size of the store, not with the value
// written, and a plugin that stores a lot makes every other plugin's writes
// slower. Measured with 500 keys resident:
//
//	Set, durable      ~340 µs   97 KB   537 allocs
//	Set, memory-only   ~25 µs   42 KB     8 allocs
//
// The disk transaction is roughly nine tenths of that. A per-request Set is
// therefore the wrong shape for this package — that is not a tuning gap, it is
// what "replaced atomically" costs — so this doc no longer offers a
// rate-limiter's counters as an example. Keep per-request counters in
// env.cache_*, which is what that is for, and use this for the state that has
// to survive a restart.
package pluginstate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	// DefaultMaxValueBytes bounds one entry. Plugin state is metadata and small
	// documents, not blobs; a plugin that needs more should be storing a
	// reference to something else.
	DefaultMaxValueBytes = 256 << 10 // 256 KiB

	// DefaultMaxTotalBytes bounds the whole store across all plugins, so a
	// buggy plugin cannot fill the disk.
	DefaultMaxTotalBytes = 32 << 20 // 32 MiB

	// DefaultMaxKeysPerPlugin bounds key count per plugin, so a plugin writing
	// unbounded distinct keys is stopped before the byte cap alone would.
	DefaultMaxKeysPerPlugin = 10_000
)

// Store is a durable, plugin-namespaced key/value store.
type Store struct {
	mu sync.RWMutex
	// flushMu serializes durable snapshots. Without it, two Set calls could
	// write snapshots concurrently and an older snapshot could rename last.
	flushMu sync.Mutex
	data    map[string]map[string]string // plugin → key → value

	path             string
	maxValueBytes    int
	maxTotalBytes    int
	maxKeysPerPlugin int
	totalBytes       int
}

// Options configures a Store. Zero values select the defaults above.
type Options struct {
	// Path is the JSON file backing the store. Empty means memory-only, which
	// is the right behaviour for tests and for a proxy with no data directory.
	Path             string
	MaxValueBytes    int
	MaxTotalBytes    int
	MaxKeysPerPlugin int
}

// New opens a store, loading any existing state from disk.
//
// A corrupt or unreadable file is reported but does not prevent startup: plugin
// state is a convenience, and refusing to boot the proxy because one plugin's
// scratch file was truncated would be a poor trade.
func New(opts Options) (*Store, error) {
	if opts.MaxValueBytes <= 0 {
		opts.MaxValueBytes = DefaultMaxValueBytes
	}
	if opts.MaxTotalBytes <= 0 {
		opts.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if opts.MaxKeysPerPlugin <= 0 {
		opts.MaxKeysPerPlugin = DefaultMaxKeysPerPlugin
	}
	s := &Store{
		data:             make(map[string]map[string]string),
		path:             opts.Path,
		maxValueBytes:    opts.MaxValueBytes,
		maxTotalBytes:    opts.MaxTotalBytes,
		maxKeysPerPlugin: opts.MaxKeysPerPlugin,
	}
	if s.path == "" {
		return s, nil
	}
	if err := s.load(); err != nil {
		return s, fmt.Errorf("plugin state: %w", err)
	}
	return s, nil
}

// Get returns one value for one plugin.
func (s *Store) Get(plugin, key string) (string, bool) {
	if s == nil || plugin == "" || key == "" {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[plugin][key]
	return v, ok
}

// Set stores a value, replacing any previous one.
//
// An empty value STORES an empty value. It used to delete the key, which made
// storing an empty string impossible and contradicted the meta and cache
// stores. Deletion is Delete.
func (s *Store) Set(plugin, key, value string) error {
	if s == nil {
		return fmt.Errorf("state store not configured")
	}
	if plugin == "" || key == "" {
		return fmt.Errorf("plugin and key are required")
	}
	if len(value) > s.maxValueBytes {
		return fmt.Errorf("value is %d bytes, limit is %d", len(value), s.maxValueBytes)
	}

	// Serialize the full candidate→durable-write→publish transaction. Readers
	// keep seeing the previous committed generation while persistence runs.
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.RLock()
	candidate := cloneData(s.data)
	totalBytes := s.totalBytes
	s.mu.RUnlock()
	bucket, ok := candidate[plugin]
	if !ok {
		bucket = make(map[string]string)
		candidate[plugin] = bucket
	}
	old, existed := bucket[key]
	if !existed && len(bucket) >= s.maxKeysPerPlugin {
		return fmt.Errorf("plugin %q already holds %d keys, the per-plugin limit", plugin, s.maxKeysPerPlugin)
	}
	delta := entrySize(key, value)
	if existed {
		delta -= entrySize(key, old)
	}
	if totalBytes+delta > s.maxTotalBytes {
		return fmt.Errorf("store would exceed its %d byte limit", s.maxTotalBytes)
	}
	bucket[key] = value
	totalBytes += delta
	if err := s.persist(candidate); err != nil {
		return err
	}
	s.mu.Lock()
	s.data = candidate
	s.totalBytes = totalBytes
	s.mu.Unlock()
	return nil
}

// Delete releases one key.
//
// Deleting a key that does not exist succeeds: the caller wants the key gone,
// and reporting an error would make every cleanup path branch on a condition
// it does not care about.
func (s *Store) Delete(plugin, key string) error {
	if s == nil {
		return fmt.Errorf("state store not configured")
	}
	if plugin == "" || key == "" {
		return fmt.Errorf("plugin and key are required")
	}

	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.RLock()
	candidate := cloneData(s.data)
	totalBytes := s.totalBytes
	s.mu.RUnlock()
	changed := false
	if old, ok := candidate[plugin][key]; ok {
		totalBytes -= entrySize(key, old)
		delete(candidate[plugin], key)
		if len(candidate[plugin]) == 0 {
			delete(candidate, plugin)
		}
		changed = true
	}
	if !changed {
		return nil
	}
	if err := s.persist(candidate); err != nil {
		return err
	}
	s.mu.Lock()
	s.data = candidate
	s.totalBytes = totalBytes
	s.mu.Unlock()
	return nil
}

// Keys lists one plugin's keys, sorted. Plugins need this to iterate state
// whose keys they did not choose — a warming plugin enumerating conversations,
// for instance.
func (s *Store) Keys(plugin string) []string {
	if s == nil || plugin == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	bucket := s.data[plugin]
	if len(bucket) == 0 {
		return nil
	}
	keys := make([]string, 0, len(bucket))
	for k := range bucket {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Len reports how many keys a plugin holds.
func (s *Store) Len(plugin string) int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data[plugin])
}

// TotalBytes reports the store's accounted size.
func (s *Store) TotalBytes() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.totalBytes
}

// entrySize accounts key and value together, so a plugin cannot evade the byte
// cap by storing everything in enormous key names.
func entrySize(key, value string) int { return len(key) + len(value) }

func cloneData(src map[string]map[string]string) map[string]map[string]string {
	out := make(map[string]map[string]string, len(src))
	for plugin, bucket := range src {
		copyBucket := make(map[string]string, len(bucket))
		for key, value := range bucket {
			copyBucket[key] = value
		}
		out[plugin] = copyBucket
	}
	return out
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", s.path, err)
	}
	var data map[string]map[string]string
	if err := json.Unmarshal(raw, &data); err != nil {
		// Keep going with an empty store rather than failing startup.
		return fmt.Errorf("parse %s (starting with empty state): %w", s.path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = data
	if s.data == nil {
		s.data = make(map[string]map[string]string)
	}
	s.totalBytes = 0
	for _, bucket := range s.data {
		for k, v := range bucket {
			s.totalBytes += entrySize(k, v)
		}
	}
	return nil
}

// persist writes a candidate state without changing the visible in-memory
// state. The caller holds flushMu.
func (s *Store) persist(candidate map[string]map[string]string) error {
	if s.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		return fmt.Errorf("plugin state: encode: %w", err)
	}
	return s.persistBytes(raw)
}

func (s *Store) persistBytes(raw []byte) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("plugin state: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("plugin state: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("plugin state: chmod: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("plugin state: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("plugin state: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("plugin state: close: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("plugin state: replace %s: %w", s.path, err)
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("plugin state: open directory %s: %w", dir, err)
	}
	if err := dirHandle.Sync(); err != nil {
		_ = dirHandle.Close()
		return fmt.Errorf("plugin state: sync directory %s: %w", dir, err)
	}
	if err := dirHandle.Close(); err != nil {
		return fmt.Errorf("plugin state: close directory %s: %w", dir, err)
	}
	return nil
}

// Namespaced renders a plugin/key pair for logs and errors.
func Namespaced(plugin, key string) string {
	return strings.Join([]string{plugin, key}, "/")
}
