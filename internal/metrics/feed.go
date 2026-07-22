// Package metrics provides tracking for Torana Edge cost savings.
// All counters are safe for concurrent use via sync/atomic; the per-plugin
// savings map is mutex-guarded.
package metrics

import "sync"

// defaultFeedCapacity is the default number of events the feed retains.
const defaultFeedCapacity = 200

// subscriberBufSize is the per-subscriber channel buffer. Subscribers that
// fall behind this many events are considered slow and have events dropped
// rather than blocking Add.
const subscriberBufSize = 64

// RequestEvent is a single per-request observability record emitted after
// every proxied call. All fields are safe to read concurrently; the struct
// is copied by value into Snapshot and subscriber channels.
type RequestEvent struct {
	// Timestamp is the wall-clock time the request completed (RFC3339Nano).
	Timestamp string `json:"timestamp"`
	// Provider is the configured provider name (e.g. "anthropic", "openai").
	Provider string `json:"provider"`
	// Model is the model name sent to the provider (e.g. "claude-3-5-sonnet").
	Model string `json:"model"`
	// Status is the HTTP status code returned to the caller.
	Status int `json:"status"`
	// LatencyMS is the total handler latency in milliseconds (wall clock from
	// the first byte of the caller's request to the last byte written).
	LatencyMS float64 `json:"latency_ms"`
	// TokensIn / TokensOut are provider-reported token counts; zero when the
	// provider did not report them or the request was vetoed before upstream.
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`
	// CacheReadTokens / CacheWriteTokens are provider prompt-cache counters;
	// zero when the provider does not support caching or nothing was cached.
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	// BytesIn / BytesOut are the raw byte counts measured on the wire
	// (request body read from caller, response body written to caller).
	BytesIn  int64 `json:"bytes_in"`
	BytesOut int64 `json:"bytes_out"`
	// Plugins lists the names of WASM plugins that fired for this request;
	// nil when no pipeline is loaded or no plugins ran.
	Plugins []string `json:"plugins,omitempty"`
	// Verdict records the control-plane outcome applied by the plugin
	// pipeline. One of: "" (normal upstream call), "block"
	// (env.block_request), "respond" (env.respond_request), "route"
	// (env.route_request). Empty when no pipeline is loaded.
	Verdict string `json:"verdict,omitempty"`
}

// subscriber holds one SSE client's channel and its unique ID used for
// unsubscription.
type subscriber struct {
	ch chan RequestEvent
}

// RequestFeed is a fixed-capacity ring buffer of recent RequestEvents
// suitable for a control-plane dashboard. It is safe for concurrent use.
//
// # Ordering
//
// Snapshot returns events newest-first (index 0 = most recent).
//
// # Slow-subscriber policy
//
// Each SSE subscriber gets a buffered channel of size [subscriberBufSize].
// Add uses a non-blocking send; if the channel is full the event is silently
// dropped for that subscriber. The ring buffer itself is never blocked by any
// subscriber.
type RequestFeed struct {
	mu          sync.Mutex
	buf         []RequestEvent
	head        int // index of the next write slot (oldest when full)
	count       int // number of valid entries (0..cap)
	cap         int
	subscribers map[uint64]*subscriber
	nextSubID   uint64
}

// NewRequestFeed creates a RequestFeed with the given capacity. If capacity
// is <= 0 the [defaultFeedCapacity] is used.
func NewRequestFeed(capacity int) *RequestFeed {
	if capacity <= 0 {
		capacity = defaultFeedCapacity
	}
	return &RequestFeed{
		buf:         make([]RequestEvent, capacity),
		cap:         capacity,
		subscribers: make(map[uint64]*subscriber),
	}
}

// Add appends ev to the ring buffer, evicting the oldest entry when full,
// and broadcasts ev to all current subscribers via non-blocking sends.
// Add is O(1) and never blocks regardless of subscriber state.
func (f *RequestFeed) Add(ev RequestEvent) {
	f.mu.Lock()
	// Write into the current head slot and advance.
	f.buf[f.head] = ev
	f.head = (f.head + 1) % f.cap
	if f.count < f.cap {
		f.count++
	}
	// Non-blocking broadcast to all subscribers.
	for _, sub := range f.subscribers {
		select {
		case sub.ch <- ev:
		default:
			// Subscriber is full; drop rather than block the hot path.
		}
	}
	f.mu.Unlock()
}

// Snapshot returns a copy of all buffered events ordered newest-first
// (index 0 = most recent event). The returned slice is a fresh allocation
// safe to hold after the lock is released.
func (f *RequestFeed) Snapshot() []RequestEvent {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.count == 0 {
		return nil
	}
	out := make([]RequestEvent, f.count)
	// newest-first: iterate backwards from the most-recently-written slot.
	for i := 0; i < f.count; i++ {
		// The slot just before head is the newest; head itself is the oldest
		// when the buffer is full.
		idx := (f.head - 1 - i + f.cap) % f.cap
		out[i] = f.buf[idx]
	}
	return out
}

// Subscribe registers a new SSE subscriber. It returns a receive-only channel
// on which future events will be delivered and an unsubscribe function that
// the caller MUST invoke when it is done (e.g. on client disconnect). Calling
// the unsubscribe function more than once is safe.
func (f *RequestFeed) Subscribe() (<-chan RequestEvent, func()) {
	_, ch, unsub := f.SubscribeWithSnapshot()
	return ch, unsub
}

// SubscribeWithSnapshot registers a new SSE subscriber and atomically captures
// a snapshot of recent events (newest-first) under the same lock, preventing
// event duplication between snapshot replay and live event stream.
func (f *RequestFeed) SubscribeWithSnapshot() ([]RequestEvent, <-chan RequestEvent, func()) {
	ch := make(chan RequestEvent, subscriberBufSize)
	f.mu.Lock()
	id := f.nextSubID
	f.nextSubID++
	f.subscribers[id] = &subscriber{ch: ch}

	var snap []RequestEvent
	if f.count > 0 {
		snap = make([]RequestEvent, f.count)
		for i := 0; i < f.count; i++ {
			idx := (f.head - 1 - i + f.cap) % f.cap
			snap[i] = f.buf[idx]
		}
	}
	f.mu.Unlock()

	once := sync.Once{}
	unsub := func() {
		once.Do(func() {
			f.mu.Lock()
			delete(f.subscribers, id)
			f.mu.Unlock()
			// Closing the channel signals the SSE handler loop that there
			// will be no more events from this subscription.
			close(ch)
		})
	}
	return snap, ch, unsub
}
