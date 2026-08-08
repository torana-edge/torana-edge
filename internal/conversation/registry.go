// Package conversation tracks which conversations have recently passed through
// the proxy, so an operator can find one by name and a plugin can act on it.
//
// Torana is otherwise stateless between requests: the client owns the history
// and the proxy forwards it. That is the right default, but it means nothing can
// answer "which conversations are active right now" — the question a cache
// plugin has to answer before it can do anything useful, and the question an
// operator has to answer before choosing one from a list.
//
// # Metadata only
//
// A record holds identifiers, timestamps, and token counts. It never holds
// message content, not even a truncated snippet. Torana's promise is that
// prompts pass through rather than accumulate, and a registry of prompt text
// sitting in host memory (and rendered into the control plane) would weaken that
// for a convenience. Plugins that need the actual bytes store them under their
// own namespaced state, where the operator has granted that explicitly.
package conversation

import (
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// DefaultMaxRecords bounds memory for a long-running proxy. Records are
	// small, but unbounded growth keyed by user-influenced input is how a
	// convenience becomes a leak.
	DefaultMaxRecords = 500

	// DefaultIdleTTL is how long a conversation stays listed after its last
	// turn. Long enough to cover a lunch break, short enough that yesterday's
	// work is not still cluttering the list.
	DefaultIdleTTL = 6 * time.Hour

	// Identity fields are host-generated hashes and must remain exact. Metadata
	// is display/replay context, so it is UTF-8-normalized and byte-bounded at
	// the registry boundary. The record-count cap alone is not a memory bound
	// when a request may carry a multi-megabyte model string.
	MaxIdentityBytes = 128
	MaxProviderBytes = 128
	MaxModelBytes    = 512
	MaxFormatBytes   = 64
	MaxPathBytes     = 4096

	janitorInterval = time.Minute
)

// Record is what the registry knows about one conversation.
type Record struct {
	// ID is engine.ConversationID: the durable label, stable as turns
	// accumulate. This is what an operator selects and what config refers to.
	ID string `json:"id"`

	// CachePrefixKey is engine.CachePrefixKey: the provider-side cache entry
	// this conversation would currently hit. Unlike ID it moves whenever the
	// prefix or model changes, which is exactly when the old entry died.
	CachePrefixKey string `json:"cache_prefix_key"`

	Provider string `json:"provider"`
	Model    string `json:"model"`
	Format   string `json:"format"`

	// Path is the caller's provider-stripped request path. Recorded because the
	// proxy never synthesizes a chat path — it reuses whatever the caller sent —
	// so anything replaying a request for this conversation needs the original.
	// It also covers shapes a format-to-path table would get wrong, like
	// Bedrock's :invoke and Code Assist's :generateContent.
	Path string `json:"path"`

	FirstSeen  time.Time `json:"first_seen"`
	LastActive time.Time `json:"last_active"`
	Turns      int       `json:"turns"`

	// Cache token counts from the most recent turn, as reported by the
	// provider. These are the ground truth for whether a cache was warm: a
	// resume showing reads hit the cache, one showing writes had to rebuild it.
	LastCacheRead  int `json:"last_cache_read"`
	LastCacheWrite int `json:"last_cache_write"`
}

// Observation is one turn's worth of facts, as the request path sees them.
type Observation struct {
	ID             string
	CachePrefixKey string
	Provider       string
	Model          string
	Format         string
	Path           string
	CacheRead      int
	CacheWrite     int
}

// Registry is a bounded, mutex-guarded set of recent conversations.
//
// The whole structure is guarded by one mutex rather than the atomics used
// elsewhere for counters: entries are compound and are read as a unit, and
// Observe runs once per request rather than per token, so contention is not a
// concern at this granularity.
type Registry struct {
	mu      sync.Mutex
	records map[string]*Record

	maxRecords int
	idleTTL    time.Duration
	now        func() time.Time // injectable so tests need not sleep

	stop     chan struct{}
	stopOnce sync.Once
}

// Options configures a Registry. The zero value selects the defaults.
type Options struct {
	MaxRecords int
	IdleTTL    time.Duration
	Now        func() time.Time
}

// New returns a Registry with a running janitor. Close stops it.
func New(opts Options) *Registry {
	if opts.MaxRecords <= 0 {
		opts.MaxRecords = DefaultMaxRecords
	}
	if opts.IdleTTL <= 0 {
		opts.IdleTTL = DefaultIdleTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	r := &Registry{
		records:    make(map[string]*Record),
		maxRecords: opts.MaxRecords,
		idleTTL:    opts.IdleTTL,
		now:        opts.Now,
		stop:       make(chan struct{}),
	}
	go r.janitor()
	return r
}

// Observe records one turn. An empty ID is ignored: engine.ConversationID
// returns "" for a request it cannot identify, and bucketing all of those under
// one key would invent a conversation that does not exist.
func (r *Registry) Observe(obs Observation) {
	if r == nil || !validIdentity(obs.ID) || (obs.CachePrefixKey != "" && !validIdentity(obs.CachePrefixKey)) {
		return
	}
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.records[obs.ID]
	if !ok {
		rec = &Record{ID: obs.ID, FirstSeen: now}
		r.records[obs.ID] = rec
	}
	rec.CachePrefixKey = obs.CachePrefixKey
	rec.Provider = boundedMetadata(obs.Provider, MaxProviderBytes)
	rec.Model = boundedMetadata(obs.Model, MaxModelBytes)
	rec.Format = boundedMetadata(obs.Format, MaxFormatBytes)
	if obs.Path != "" {
		rec.Path = boundedMetadata(obs.Path, MaxPathBytes)
	}
	rec.LastActive = now
	rec.Turns++
	rec.LastCacheRead = obs.CacheRead
	rec.LastCacheWrite = obs.CacheWrite

	r.evictLocked()
}

func validIdentity(value string) bool {
	return value != "" && len(value) <= MaxIdentityBytes && utf8.ValidString(value)
}

func boundedMetadata(value string, limit int) string {
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "�")
	}
	if len(value) <= limit {
		return value
	}
	const omitted = "…"
	end := limit - len(omitted)
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + omitted
}

// List returns a snapshot ordered most-recently-active first. Callers get
// copies, so a held snapshot never mutates underfoot.
func (r *Registry) List() []Record {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Record, 0, len(r.records))
	for _, rec := range r.records {
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastActive.Equal(out[j].LastActive) {
			// Stable tiebreak: two conversations observed within the same clock
			// tick must not swap order between calls.
			return out[i].ID < out[j].ID
		}
		return out[i].LastActive.After(out[j].LastActive)
	})
	return out
}

// Get returns one conversation by ID.
func (r *Registry) Get(id string) (Record, bool) {
	if r == nil || id == "" {
		return Record{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.records[id]
	if !ok {
		return Record{}, false
	}
	return *rec, true
}

// Len reports how many conversations are currently tracked.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.records)
}

// Close stops the janitor. Safe to call more than once.
func (r *Registry) Close() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() { close(r.stop) })
}

// evictLocked drops expired records, then the least recently active ones until
// the map is within its bound. Caller holds r.mu.
func (r *Registry) evictLocked() {
	cutoff := r.now().Add(-r.idleTTL)
	for id, rec := range r.records {
		if rec.LastActive.Before(cutoff) {
			delete(r.records, id)
		}
	}
	if len(r.records) <= r.maxRecords {
		return
	}
	// Over the bound: sort what is left and drop from the stale end. This runs
	// only when the cap is exceeded, so the sort is not on the hot path.
	ids := make([]string, 0, len(r.records))
	for id := range r.records {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := r.records[ids[i]], r.records[ids[j]]
		if a.LastActive.Equal(b.LastActive) {
			return a.ID < b.ID
		}
		return a.LastActive.Before(b.LastActive)
	})
	for _, id := range ids[:len(r.records)-r.maxRecords] {
		delete(r.records, id)
	}
}

// janitor expires idle records even while no traffic arrives, so a quiet proxy
// does not keep yesterday's conversations listed indefinitely.
func (r *Registry) janitor() {
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.mu.Lock()
			r.evictLocked()
			r.mu.Unlock()
		}
	}
}
