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
// slower. Measured with 500 keys resident (BenchmarkSet* in this package):
//
//	Set, durable            ~500 µs   99 KB   537 allocs
//	  of which, serializing ~280 µs   53 KB   507 allocs
//	Set, memory-only         ~42 µs   42 KB     8 allocs
//
// Serializing the whole store is the dominant term — about half the time and
// nearly all of the allocations — with the filesystem transaction behind it.
// A per-request Set is therefore the wrong shape for this package. That is not
// a tuning gap: it is what "one JSON file, replaced atomically" costs.
//
// Which is why this doc no longer offers a rate-limiter's counters as an
// example. Note that env.cache_* is not the answer for counters either: it
// exposes Get and Set and no atomic increment, so a read-modify-write counter
// loses updates whenever two requests overlap. Nothing here provides safe
// concurrent counters today. Use this package for state that must survive a
// restart and is written occasionally, and keep genuinely per-request data in
// request-scoped env.meta_*.
package pluginstate

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	flushMu  sync.Mutex
	data     map[string]map[string]string // plugin → key → value
	versions map[string]map[string]string
	counter  uint64

	path             string
	maxValueBytes    int
	maxTotalBytes    int
	maxKeysPerPlugin int
	totalBytes       int
	readOnly         atomic.Bool
	// afterRename is a test fault hook; set before sharing the store.
	afterRename func() error
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
		versions:         make(map[string]map[string]string),
		path:             opts.Path,
		maxValueBytes:    opts.MaxValueBytes,
		maxTotalBytes:    opts.MaxTotalBytes,
		maxKeysPerPlugin: opts.MaxKeysPerPlugin,
	}
	if s.path == "" {
		return s, nil
	}
	if err := s.load(); err != nil {
		s.readOnly.Store(true)
		s.data = make(map[string]map[string]string)
		s.versions = make(map[string]map[string]string)
		s.totalBytes = 0
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

// PageEntry is one durable state entry including its opaque version token.
type PageEntry struct{ Key, Value, Version string }

type stateEnvelope struct {
	Format  int                             `json:"format"`
	Counter uint64                          `json:"counter"`
	Data    map[string]map[string]PageEntry `json:"data"`
}
type cursorToken struct{ Plugin, Prefix, Last string }

func (s *Store) GetVersioned(plugin, key string) (string, string, bool) {
	if s == nil || plugin == "" || key == "" {
		return "", "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[plugin][key]
	if !ok {
		return "", "", false
	}
	return v, s.versions[plugin][key], true
}

func (s *Store) nextVersion() (string, error) {
	if s.counter == ^uint64(0) {
		return "", ErrVersionExhausted
	}
	s.counter++
	return strconv.FormatUint(s.counter, 10), nil
}

var ErrVersionExhausted = errors.New("plugin state version counter exhausted")

func (s *Store) CompareAndSet(plugin, key, value string, expected *string) (bool, string, error) {
	if s == nil {
		return false, "", errors.New("state store not configured")
	}
	if s.readOnly.Load() {
		return false, "", errors.New("plugin state is read-only after corrupt load")
	}
	if plugin == "" || key == "" {
		return false, "", errors.New("plugin and key are required")
	}
	if len(value) > s.maxValueBytes {
		return false, "", fmt.Errorf("value is %d bytes, limit is %d", len(value), s.maxValueBytes)
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	if s.readOnly.Load() {
		return false, "", errors.New("state store requires reopen after a persistence failure")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, exists := s.versions[plugin][key]
	if (expected == nil && exists) || (expected != nil && (!exists || *expected != cur)) {
		return false, "", nil
	}
	if !exists && len(s.data[plugin]) >= s.maxKeysPerPlugin {
		return false, "", fmt.Errorf("plugin %q already holds %d keys, the per-plugin limit", plugin, s.maxKeysPerPlugin)
	}
	if s.counter == ^uint64(0) {
		return false, "", ErrVersionExhausted
	}
	candidate := cloneData(s.data)
	vers := cloneData(s.versions)
	total := s.totalBytes
	if candidate[plugin] == nil {
		candidate[plugin] = map[string]string{}
	}
	if vers[plugin] == nil {
		vers[plugin] = map[string]string{}
	}
	if exists {
		total -= entrySize(key, curValue(s.data, plugin, key))
	}
	if total+entrySize(key, value) > s.maxTotalBytes {
		return false, "", fmt.Errorf("store would exceed its %d byte limit", s.maxTotalBytes)
	}
	version, _ := s.nextVersion()
	candidate[plugin][key] = value
	vers[plugin][key] = version
	if err := s.persistVersioned(candidate, vers, s.counter); err != nil {
		// The rename may already have happened before a later fsync error;
		// consume the token permanently to prevent ABA on retry.
		return false, "", err
	}
	s.data, s.versions, s.totalBytes = candidate, vers, total+entrySize(key, value)
	return true, version, nil
}

func curValue(d map[string]map[string]string, p, k string) string { return d[p][k] }

func (s *Store) CompareAndDelete(plugin, key, expected string) (bool, error) {
	if s == nil {
		return false, errors.New("state store not configured")
	}
	if plugin == "" || key == "" || expected == "" {
		return false, errors.New("plugin, key, and expected version are required")
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	if s.readOnly.Load() {
		return false, errors.New("state store requires reopen after a persistence failure")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[plugin][key]; !ok || s.versions[plugin][key] != expected {
		return false, nil
	}
	candidate := cloneData(s.data)
	vers := cloneData(s.versions)
	total := s.totalBytes - totalEntry(candidate, plugin, key)
	delete(candidate[plugin], key)
	delete(vers[plugin], key)
	if len(candidate[plugin]) == 0 {
		delete(candidate, plugin)
		delete(vers, plugin)
	}
	if err := s.persistVersioned(candidate, vers, s.counter); err != nil {
		return false, err
	}
	s.data, s.versions, s.totalBytes = candidate, vers, total
	return true, nil
}

func totalEntry(d map[string]map[string]string, p, k string) int {
	if v, ok := d[p][k]; ok {
		return entrySize(k, v)
	}
	return 0
}

func (s *Store) Scan(plugin, prefix, cursor string, limit, maxBytes int) ([]PageEntry, string, error) {
	if s == nil || plugin == "" {
		return nil, "", errors.New("plugin is required")
	}
	if limit < 1 || limit > 256 || maxBytes < 1 {
		return nil, "", errors.New("invalid scan bounds")
	}
	last := ""
	if cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil {
			return nil, "", errors.New("invalid cursor")
		}
		var c cursorToken
		if json.Unmarshal(raw, &c) != nil || c.Plugin != plugin || c.Prefix != prefix {
			return nil, "", errors.New("invalid cursor")
		}
		last = c.Last
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0)
	for k := range s.data[plugin] {
		if strings.HasPrefix(k, prefix) && k > last {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]PageEntry, 0, limit)
	bytes := 0
	for _, k := range keys {
		v := s.data[plugin][k]
		e := PageEntry{k, v, s.versions[plugin][k]}
		n := len(k) + len(v) + len(e.Version)
		if len(out) == 0 && n > maxBytes {
			return nil, "", errors.New("first scan entry exceeds byte budget")
		}
		if len(out) >= limit || bytes+n > maxBytes {
			break
		}
		out = append(out, e)
		bytes += n
		last = k
	}
	if len(out) == 0 || last == "" {
		return out, "", nil
	}
	more := len(keys) > 0 && last < keys[len(keys)-1]
	if !more {
		return out, "", nil
	}
	raw, _ := json.Marshal(cursorToken{plugin, prefix, last})
	return out, base64.RawURLEncoding.EncodeToString(raw), nil
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
	if s.readOnly.Load() {
		return errors.New("plugin state is read-only after corrupt load")
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
	if s.readOnly.Load() {
		return errors.New("state store requires reopen after a persistence failure")
	}
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
	s.mu.Lock()
	version, verr := s.nextVersion()
	s.mu.Unlock()
	if verr != nil {
		return verr
	}
	versions := cloneData(s.versions)
	if versions[plugin] == nil {
		versions[plugin] = map[string]string{}
	}
	versions[plugin][key] = version
	if err := s.persistVersioned(candidate, versions, s.counter); err != nil {
		// Never reuse a token after an ambiguous filesystem failure.
		return err
	}
	s.mu.Lock()
	s.data = candidate
	s.versions = versions
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
	if s.readOnly.Load() {
		return errors.New("plugin state is read-only after corrupt load")
	}
	if plugin == "" || key == "" {
		return fmt.Errorf("plugin and key are required")
	}

	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	if s.readOnly.Load() {
		return errors.New("state store requires reopen after a persistence failure")
	}
	s.mu.RLock()
	candidate := cloneData(s.data)
	versions := cloneData(s.versions)
	totalBytes := s.totalBytes
	s.mu.RUnlock()
	changed := false
	if old, ok := candidate[plugin][key]; ok {
		totalBytes -= entrySize(key, old)
		delete(candidate[plugin], key)
		delete(versions[plugin], key)
		if len(candidate[plugin]) == 0 {
			delete(candidate, plugin)
			delete(versions, plugin)
		}
		changed = true
	}
	if !changed {
		return nil
	}
	if err := s.persistVersioned(candidate, versions, s.counter); err != nil {
		return err
	}
	s.mu.Lock()
	s.data = candidate
	s.versions = versions
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
	var env stateEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// Keep going with an empty store rather than failing startup.
		return fmt.Errorf("parse %s (starting with empty state): %w", s.path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	legacy := false
	if env.Format == 1 && env.Data != nil {
		s.counter = env.Counter
		seen := map[string]bool{}
		for p, b := range env.Data {
			for k, e := range b {
				n, err := strconv.ParseUint(e.Version, 10, 64)
				if err != nil || e.Version == "" || n == 0 || n > s.counter || seen[e.Version] {
					return fmt.Errorf("invalid version envelope")
				}
				seen[e.Version] = true
				if s.data[p] == nil {
					s.data[p] = map[string]string{}
				}
				if s.versions[p] == nil {
					s.versions[p] = map[string]string{}
				}
				s.data[p][k] = e.Value
				s.versions[p][k] = e.Version
			}
		}
	} else {
		legacy = true
		var data map[string]map[string]string
		if err := json.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("parse %s (starting with empty state): %w", s.path, err)
		}
		s.data = data
		if s.data == nil {
			s.data = map[string]map[string]string{}
		}
		s.versions = make(map[string]map[string]string)
		for p, b := range s.data {
			s.versions[p] = map[string]string{}
			for k := range b {
				s.counter++
				s.versions[p][k] = strconv.FormatUint(s.counter, 10)
			}
		}
	}
	s.totalBytes = 0
	for _, bucket := range s.data {
		for k, v := range bucket {
			s.totalBytes += entrySize(k, v)
		}
	}
	if legacy {
		if err := s.persistVersioned(s.data, s.versions, s.counter); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) persistVersioned(candidate map[string]map[string]string, versions map[string]map[string]string, counter uint64) error {
	if s.path == "" {
		return nil
	}
	env := stateEnvelope{Format: 1, Counter: counter, Data: make(map[string]map[string]PageEntry)}
	for p, b := range candidate {
		env.Data[p] = map[string]PageEntry{}
		for k, v := range b {
			env.Data[p][k] = PageEntry{k, v, versions[p][k]}
		}
	}
	raw, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("plugin state: encode: %w", err)
	}
	return s.persistBytes(raw)
}

func (s *Store) persistBytes(raw []byte) (result error) {
	published := false
	defer func() {
		// Once rename succeeds, disk may contain the candidate even if directory
		// sync fails. Refuse all further writes until reopen reconciles that state;
		// stale in-memory values must never authorize a subsequent CAS.
		if published && result != nil {
			s.readOnly.Store(true)
		}
	}()

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
	published = true
	if s.afterRename != nil {
		if err := s.afterRename(); err != nil {
			return err
		}
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
