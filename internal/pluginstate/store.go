// Package pluginstate provides durable, plugin-private key/value storage.
//
// Persistent stores use bbolt: one write updates only the affected key and its
// accounting metadata in a single fsync-backed transaction. This is suitable
// for per-tool-result ledgers and other request-path state; it replaces the
// original whole-file JSON snapshot whose write cost grew with every key in
// every plugin. An empty Path keeps the in-memory implementation used by tests.
package pluginstate

import (
	"encoding/base64"
	"encoding/binary"
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
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	DefaultMaxValueBytes    = 256 << 10
	DefaultMaxTotalBytes    = 32 << 20
	DefaultMaxKeysPerPlugin = 10_000
)

var (
	bucketMetadata      = []byte("metadata")
	bucketPlugins       = []byte("plugins")
	bucketPluginCounts  = []byte("plugin_key_counts")
	keyCounter          = []byte("counter")
	keyTotalBytes       = []byte("total_bytes")
	ErrVersionExhausted = errors.New("plugin state version counter exhausted")
)

// Store is a durable, plugin-namespaced key/value store. When db is nil and
// path is empty, the maps are the memory-only backend.
type Store struct {
	mu       sync.RWMutex
	data     map[string]map[string]string
	versions map[string]map[string]string
	counter  uint64
	total    int

	db               *bolt.DB
	path             string
	maxValueBytes    int
	maxTotalBytes    int
	maxKeysPerPlugin int
	readOnly         atomic.Bool
}

// Options configures a Store.
type Options struct {
	Path             string
	MaxValueBytes    int
	MaxTotalBytes    int
	MaxKeysPerPlugin int
}

type PageEntry struct{ Key, Value, Version string }

type cursorToken struct{ Plugin, Prefix, Last string }

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
		data:             map[string]map[string]string{},
		versions:         map[string]map[string]string{},
		path:             opts.Path,
		maxValueBytes:    opts.MaxValueBytes,
		maxTotalBytes:    opts.MaxTotalBytes,
		maxKeysPerPlugin: opts.MaxKeysPerPlugin,
	}
	if opts.Path == "" {
		return s, nil
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o700); err != nil {
		s.readOnly.Store(true)
		return s, fmt.Errorf("plugin state: create directory: %w", err)
	}
	db, err := bolt.Open(opts.Path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		s.readOnly.Store(true)
		return s, fmt.Errorf("plugin state: open %s: %w", opts.Path, err)
	}
	s.db = db
	if err := os.Chmod(opts.Path, 0o600); err != nil {
		_ = db.Close()
		s.db = nil
		s.readOnly.Store(true)
		return s, fmt.Errorf("plugin state: chmod %s: %w", opts.Path, err)
	}
	if err := s.initialize(); err != nil {
		_ = db.Close()
		s.db = nil
		s.readOnly.Store(true)
		return s, fmt.Errorf("plugin state: %w", err)
	}
	return s, nil
}

func (s *Store) initialize() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(bucketMetadata)
		if err != nil {
			return err
		}
		_, err = tx.CreateBucketIfNotExists(bucketPlugins)
		if err != nil {
			return err
		}
		if meta.Get(keyCounter) == nil {
			if err := putUint64(meta, keyCounter, 0); err != nil {
				return err
			}
		}
		if meta.Get(keyTotalBytes) == nil {
			if err := putUint64(meta, keyTotalBytes, 0); err != nil {
				return err
			}
		}
		counts, err := tx.CreateBucketIfNotExists(bucketPluginCounts)
		if err != nil {
			return err
		}
		return s.validateDatabase(meta, tx.Bucket(bucketPlugins), counts)
	})
}

func (s *Store) validateDatabase(meta, plugins, counts *bolt.Bucket) error {
	if meta == nil || plugins == nil || counts == nil || len(meta.Get(keyCounter)) != 8 || len(meta.Get(keyTotalBytes)) != 8 {
		return errors.New("invalid state metadata")
	}
	var total uint64
	var highestVersion uint64
	err := plugins.ForEach(func(pluginName, value []byte) error {
		if len(pluginName) == 0 || value != nil {
			return errors.New("invalid plugin bucket")
		}
		bucket := plugins.Bucket(pluginName)
		if bucket == nil {
			return errors.New("missing plugin bucket")
		}
		countRaw := counts.Get(pluginName)
		if len(countRaw) != 8 {
			return fmt.Errorf("plugin %q has missing key-count metadata", pluginName)
		}
		declaredCount := readUint64(countRaw)
		if declaredCount > uint64(s.maxKeysPerPlugin) {
			return fmt.Errorf("plugin %q exceeds its %d key limit", pluginName, s.maxKeysPerPlugin)
		}
		var actualCount uint64
		return bucket.ForEach(func(key, raw []byte) error {
			if len(key) == 0 || raw == nil {
				return fmt.Errorf("plugin %q contains an invalid entry", pluginName)
			}
			version, text, ok := decodeRecord(raw)
			if !ok {
				return fmt.Errorf("plugin %q key %q contains an invalid record", pluginName, key)
			}
			if len(text) > s.maxValueBytes {
				return fmt.Errorf("plugin %q key %q exceeds its %d byte value limit", pluginName, key, s.maxValueBytes)
			}
			size := uint64(len(key) + len(text))
			if ^uint64(0)-total < size {
				return errors.New("state size overflow")
			}
			total += size
			actualCount++
			if version > highestVersion {
				highestVersion = version
			}
			if actualCount > declaredCount {
				return fmt.Errorf("plugin %q key-count accounting mismatch", pluginName)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	if err := counts.ForEach(func(pluginName, raw []byte) error {
		if len(pluginName) == 0 || len(raw) != 8 || plugins.Bucket(pluginName) == nil {
			return errors.New("invalid plugin key-count metadata")
		}
		if uint64(plugins.Bucket(pluginName).Stats().KeyN) != readUint64(raw) {
			return fmt.Errorf("plugin %q key-count accounting mismatch", pluginName)
		}
		return nil
	}); err != nil {
		return err
	}
	if total != readUint64(meta.Get(keyTotalBytes)) {
		return errors.New("state byte accounting mismatch")
	}
	if total > uint64(s.maxTotalBytes) {
		return fmt.Errorf("state exceeds its %d byte limit", s.maxTotalBytes)
	}
	if highestVersion > readUint64(meta.Get(keyCounter)) {
		return errors.New("state version counter is behind a stored record")
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *Store) ReadOnly() bool { return s != nil && s.readOnly.Load() }

func (s *Store) Get(plugin, key string) (string, bool, error) {
	value, _, found, err := s.GetVersioned(plugin, key)
	return value, found, err
}

func (s *Store) GetVersioned(plugin, key string) (string, string, bool, error) {
	if s == nil || plugin == "" || key == "" {
		return "", "", false, errors.New("plugin and key are required")
	}
	if s.readOnly.Load() || (s.db == nil && s.path != "") {
		return "", "", false, errors.New("state store not configured")
	}
	if s.db == nil {
		s.mu.RLock()
		defer s.mu.RUnlock()
		value, found := s.data[plugin][key]
		return value, s.versions[plugin][key], found, nil
	}
	var value, version string
	found := false
	if err := s.db.View(func(tx *bolt.Tx) error {
		bucket := pluginBucket(tx, plugin)
		if bucket == nil {
			return nil
		}
		record := bucket.Get([]byte(key))
		if record == nil {
			return nil
		}
		v, text, ok := decodeRecord(record)
		if !ok {
			return errors.New("invalid state record")
		}
		value, version, found = text, strconv.FormatUint(v, 10), true
		return nil
	}); err != nil {
		return "", "", false, fmt.Errorf("read state: %w", err)
	}
	return value, version, found, nil
}

func (s *Store) Set(plugin, key, value string) error {
	_, _, err := s.set(plugin, key, value, nil, false)
	return err
}

func (s *Store) CompareAndSet(plugin, key, value string, expected *string) (bool, string, error) {
	return s.set(plugin, key, value, expected, true)
}

func (s *Store) set(plugin, key, value string, expected *string, conditional bool) (bool, string, error) {
	if err := s.validateWrite(plugin, key, value); err != nil {
		return false, "", err
	}
	if s.db == nil {
		return s.setMemory(plugin, key, value, expected, conditional)
	}
	var applied bool
	var version uint64
	err := s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMetadata)
		plugins := tx.Bucket(bucketPlugins)
		counts := tx.Bucket(bucketPluginCounts)
		bucket := plugins.Bucket([]byte(plugin))
		var oldValue string
		var oldVersion uint64
		exists := false
		if bucket != nil {
			if record := bucket.Get([]byte(key)); record != nil {
				var ok bool
				oldVersion, oldValue, ok = decodeRecord(record)
				if !ok {
					return errors.New("invalid state record")
				}
				exists = true
			}
		}
		if conditional {
			if expected == nil && exists {
				return nil
			}
			if expected != nil && (!exists || *expected != strconv.FormatUint(oldVersion, 10)) {
				return nil
			}
		}
		count := readUint64(counts.Get([]byte(plugin)))
		if bucket != nil && counts.Get([]byte(plugin)) == nil {
			return errors.New("missing plugin key-count metadata")
		}
		if !exists && count >= uint64(s.maxKeysPerPlugin) {
			return fmt.Errorf("plugin %q already holds %d keys, the per-plugin limit", plugin, s.maxKeysPerPlugin)
		}
		if bucket == nil {
			var err error
			bucket, err = plugins.CreateBucketIfNotExists([]byte(plugin))
			if err != nil {
				return err
			}
		}
		total := readUint64(meta.Get(keyTotalBytes))
		if exists {
			total -= uint64(entrySize(key, oldValue))
		}
		total += uint64(entrySize(key, value))
		if total > uint64(s.maxTotalBytes) {
			return fmt.Errorf("store would exceed its %d byte limit", s.maxTotalBytes)
		}
		counter := readUint64(meta.Get(keyCounter))
		if counter == ^uint64(0) {
			return ErrVersionExhausted
		}
		version = counter + 1
		if err := bucket.Put([]byte(key), encodeRecord(version, value)); err != nil {
			return err
		}
		if !exists {
			if err := putUint64(counts, []byte(plugin), count+1); err != nil {
				return err
			}
		}
		if err := putUint64(meta, keyCounter, version); err != nil {
			return err
		}
		if err := putUint64(meta, keyTotalBytes, total); err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, strconv.FormatUint(version, 10), err
}

func (s *Store) validateWrite(plugin, key, value string) error {
	if s == nil || s.readOnly.Load() || (s.db == nil && s.path != "") {
		return errors.New("state store not configured")
	}
	if plugin == "" || key == "" {
		return errors.New("plugin and key are required")
	}
	if len(value) > s.maxValueBytes {
		return fmt.Errorf("value is %d bytes, limit is %d", len(value), s.maxValueBytes)
	}
	return nil
}

func (s *Store) setMemory(plugin, key, value string, expected *string, conditional bool) (bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, exists := s.data[plugin][key]
	if conditional {
		if expected == nil && exists {
			return false, "", nil
		}
		if expected != nil && (!exists || s.versions[plugin][key] != *expected) {
			return false, "", nil
		}
	}
	if !exists && len(s.data[plugin]) >= s.maxKeysPerPlugin {
		return false, "", fmt.Errorf("plugin %q already holds %d keys, the per-plugin limit", plugin, s.maxKeysPerPlugin)
	}
	total := s.total + entrySize(key, value)
	if exists {
		total -= entrySize(key, old)
	}
	if total > s.maxTotalBytes {
		return false, "", fmt.Errorf("store would exceed its %d byte limit", s.maxTotalBytes)
	}
	if s.counter == ^uint64(0) {
		return false, "", ErrVersionExhausted
	}
	if s.data[plugin] == nil {
		s.data[plugin], s.versions[plugin] = map[string]string{}, map[string]string{}
	}
	s.counter++
	version := strconv.FormatUint(s.counter, 10)
	s.data[plugin][key], s.versions[plugin][key], s.total = value, version, total
	return true, version, nil
}

func (s *Store) Delete(plugin, key string) error {
	if s == nil || plugin == "" || key == "" {
		return errors.New("plugin and key are required")
	}
	if s.readOnly.Load() || (s.db == nil && s.path != "") {
		return errors.New("state store not configured")
	}
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if value, ok := s.data[plugin][key]; ok {
			s.total -= entrySize(key, value)
			delete(s.data[plugin], key)
			delete(s.versions[plugin], key)
			if len(s.data[plugin]) == 0 {
				delete(s.data, plugin)
				delete(s.versions, plugin)
			}
		}
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMetadata)
		plugins := tx.Bucket(bucketPlugins)
		counts := tx.Bucket(bucketPluginCounts)
		bucket := plugins.Bucket([]byte(plugin))
		if bucket == nil {
			return nil
		}
		record := bucket.Get([]byte(key))
		if record == nil {
			return nil
		}
		_, value, ok := decodeRecord(record)
		if !ok {
			return errors.New("invalid state record")
		}
		if err := bucket.Delete([]byte(key)); err != nil {
			return err
		}
		total := readUint64(meta.Get(keyTotalBytes)) - uint64(entrySize(key, value))
		if err := putUint64(meta, keyTotalBytes, total); err != nil {
			return err
		}
		countRaw := counts.Get([]byte(plugin))
		if len(countRaw) != 8 || readUint64(countRaw) == 0 {
			return errors.New("invalid plugin key-count metadata")
		}
		count := readUint64(countRaw) - 1
		if count == 0 {
			if err := counts.Delete([]byte(plugin)); err != nil {
				return err
			}
			return plugins.DeleteBucket([]byte(plugin))
		}
		return putUint64(counts, []byte(plugin), count)
	})
}

func (s *Store) CompareAndDelete(plugin, key, expected string) (bool, error) {
	if expected == "" {
		return false, errors.New("plugin, key, and expected version are required")
	}
	if s == nil || plugin == "" || key == "" || s.readOnly.Load() || (s.db == nil && s.path != "") {
		return false, errors.New("state store not configured")
	}
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.versions[plugin][key] != expected {
			return false, nil
		}
		value := s.data[plugin][key]
		delete(s.data[plugin], key)
		delete(s.versions[plugin], key)
		s.total -= entrySize(key, value)
		if len(s.data[plugin]) == 0 {
			delete(s.data, plugin)
			delete(s.versions, plugin)
		}
		return true, nil
	}
	applied := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMetadata)
		plugins := tx.Bucket(bucketPlugins)
		counts := tx.Bucket(bucketPluginCounts)
		bucket := plugins.Bucket([]byte(plugin))
		if bucket == nil {
			return nil
		}
		record := bucket.Get([]byte(key))
		version, value, ok := decodeRecord(record)
		if !ok || strconv.FormatUint(version, 10) != expected {
			return nil
		}
		if err := bucket.Delete([]byte(key)); err != nil {
			return err
		}
		if err := putUint64(meta, keyTotalBytes, readUint64(meta.Get(keyTotalBytes))-uint64(entrySize(key, value))); err != nil {
			return err
		}
		countRaw := counts.Get([]byte(plugin))
		if len(countRaw) != 8 || readUint64(countRaw) == 0 {
			return errors.New("invalid plugin key-count metadata")
		}
		count := readUint64(countRaw) - 1
		if count == 0 {
			if err := counts.Delete([]byte(plugin)); err != nil {
				return err
			}
			if err := plugins.DeleteBucket([]byte(plugin)); err != nil {
				return err
			}
		} else if err := putUint64(counts, []byte(plugin), count); err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, err
}

func (s *Store) Keys(plugin string) []string {
	if s == nil || plugin == "" {
		return nil
	}
	if s.db == nil {
		s.mu.RLock()
		defer s.mu.RUnlock()
		if len(s.data[plugin]) == 0 {
			return nil
		}
		keys := make([]string, 0, len(s.data[plugin]))
		for key := range s.data[plugin] {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return keys
	}
	var keys []string
	_ = s.db.View(func(tx *bolt.Tx) error {
		bucket := pluginBucket(tx, plugin)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(key, value []byte) error {
			keys = append(keys, string(key))
			return nil
		})
	})
	return keys
}

func (s *Store) Scan(plugin, prefix, cursor string, limit, maxBytes int) ([]PageEntry, string, error) {
	if s == nil || plugin == "" {
		return nil, "", errors.New("plugin is required")
	}
	if limit < 1 || limit > 256 || maxBytes < 1 {
		return nil, "", errors.New("invalid scan bounds")
	}
	last, err := decodeCursor(plugin, prefix, cursor)
	if err != nil {
		return nil, "", err
	}
	if s.db == nil {
		return s.scanMemory(plugin, prefix, last, limit, maxBytes)
	}
	var out []PageEntry
	more := false
	err = s.db.View(func(tx *bolt.Tx) error {
		bucket := pluginBucket(tx, plugin)
		if bucket == nil {
			return nil
		}
		c := bucket.Cursor()
		seek := []byte(prefix)
		if last != "" {
			seek = []byte(last)
		}
		key, record := c.Seek(seek)
		if last != "" && key != nil && string(key) == last {
			key, record = c.Next()
		}
		used := 0
		for key != nil && strings.HasPrefix(string(key), prefix) {
			version, value, ok := decodeRecord(record)
			if !ok {
				return errors.New("invalid state record")
			}
			entry := PageEntry{Key: string(key), Value: value, Version: strconv.FormatUint(version, 10)}
			size := len(entry.Key) + len(entry.Value) + len(entry.Version)
			if len(out) == 0 && size > maxBytes {
				return errors.New("first scan entry exceeds byte budget")
			}
			if len(out) >= limit || used+size > maxBytes {
				more = true
				break
			}
			out, used = append(out, entry), used+size
			key, record = c.Next()
		}
		return nil
	})
	if err != nil || len(out) == 0 || !more {
		return out, "", err
	}
	return out, encodeCursor(plugin, prefix, out[len(out)-1].Key), nil
}

func (s *Store) scanMemory(plugin, prefix, last string, limit, maxBytes int) ([]PageEntry, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0)
	for key := range s.data[plugin] {
		if strings.HasPrefix(key, prefix) && key > last {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	out := make([]PageEntry, 0, min(limit, len(keys)))
	used := 0
	for _, key := range keys {
		entry := PageEntry{Key: key, Value: s.data[plugin][key], Version: s.versions[plugin][key]}
		size := len(entry.Key) + len(entry.Value) + len(entry.Version)
		if len(out) == 0 && size > maxBytes {
			return nil, "", errors.New("first scan entry exceeds byte budget")
		}
		if len(out) >= limit || used+size > maxBytes {
			return out, encodeCursor(plugin, prefix, out[len(out)-1].Key), nil
		}
		out, used = append(out, entry), used+size
	}
	return out, "", nil
}

func (s *Store) Len(plugin string) int {
	if s == nil {
		return 0
	}
	if s.db == nil {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return len(s.data[plugin])
	}
	count := 0
	_ = s.db.View(func(tx *bolt.Tx) error {
		if bucket := pluginBucket(tx, plugin); bucket != nil {
			count = bucket.Stats().KeyN
		}
		return nil
	})
	return count
}

func (s *Store) TotalBytes() int {
	if s == nil {
		return 0
	}
	if s.db == nil {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.total
	}
	total := 0
	_ = s.db.View(func(tx *bolt.Tx) error {
		total = int(readUint64(tx.Bucket(bucketMetadata).Get(keyTotalBytes)))
		return nil
	})
	return total
}

func pluginBucket(tx *bolt.Tx, plugin string) *bolt.Bucket {
	root := tx.Bucket(bucketPlugins)
	if root == nil {
		return nil
	}
	return root.Bucket([]byte(plugin))
}

func encodeRecord(version uint64, value string) []byte {
	out := make([]byte, 8+len(value))
	binary.BigEndian.PutUint64(out[:8], version)
	copy(out[8:], value)
	return out
}

func decodeRecord(raw []byte) (uint64, string, bool) {
	if len(raw) < 8 {
		return 0, "", false
	}
	version := binary.BigEndian.Uint64(raw[:8])
	return version, string(raw[8:]), version != 0
}

func readUint64(raw []byte) uint64 {
	if len(raw) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(raw)
}

func putUint64(bucket *bolt.Bucket, key []byte, value uint64) error {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], value)
	return bucket.Put(key, raw[:])
}

func decodeCursor(plugin, prefix, cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", errors.New("invalid cursor")
	}
	var token cursorToken
	if json.Unmarshal(raw, &token) != nil || token.Plugin != plugin || token.Prefix != prefix {
		return "", errors.New("invalid cursor")
	}
	return token.Last, nil
}

func encodeCursor(plugin, prefix, last string) string {
	raw, _ := json.Marshal(cursorToken{Plugin: plugin, Prefix: prefix, Last: last})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func entrySize(key, value string) int { return len(key) + len(value) }

func Namespaced(plugin, key string) string { return strings.Join([]string{plugin, key}, "/") }
