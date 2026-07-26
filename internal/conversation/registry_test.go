package conversation

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeClock lets the eviction tests run without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestRegistry(t *testing.T, opts Options) (*Registry, *fakeClock) {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
	opts.Now = clock.now
	r := New(opts)
	t.Cleanup(r.Close)
	return r, clock
}

func obs(id string) Observation {
	return Observation{ID: id, CachePrefixKey: "prefix-" + id, Provider: "anthropic", Model: "sonnet", Path: "/v1/messages"}
}

// TestObserveAccumulatesTurns — the registry must recognise a returning
// conversation rather than creating a second record for it.
func TestObserveAccumulatesTurns(t *testing.T) {
	r, clock := newTestRegistry(t, Options{})

	r.Observe(obs("a3f9"))
	clock.advance(time.Minute)
	r.Observe(obs("a3f9"))

	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1 — a returning conversation created a new record", r.Len())
	}
	rec, ok := r.Get("a3f9")
	if !ok {
		t.Fatal("conversation missing")
	}
	if rec.Turns != 2 {
		t.Errorf("Turns = %d, want 2", rec.Turns)
	}
	if !rec.LastActive.After(rec.FirstSeen) {
		t.Error("LastActive did not advance past FirstSeen")
	}
}

// TestUnidentifiableIgnored — engine.ConversationID returns "" when it cannot
// identify a request. Bucketing those together would invent a conversation.
func TestUnidentifiableIgnored(t *testing.T) {
	r, _ := newTestRegistry(t, Options{})
	r.Observe(Observation{ID: "", Provider: "anthropic"})
	if r.Len() != 0 {
		t.Errorf("Len = %d, want 0 — an unidentifiable request was recorded", r.Len())
	}
}

// TestCachePrefixKeyTracksLatest — the label is stable but the cache key moves
// when the prefix is rewritten, and the registry must show the live one.
func TestCachePrefixKeyTracksLatest(t *testing.T) {
	r, _ := newTestRegistry(t, Options{})

	first := obs("a3f9")
	first.CachePrefixKey = "before-compaction"
	r.Observe(first)

	second := obs("a3f9")
	second.CachePrefixKey = "after-compaction"
	r.Observe(second)

	rec, _ := r.Get("a3f9")
	if rec.CachePrefixKey != "after-compaction" {
		t.Errorf("CachePrefixKey = %q, want the current one", rec.CachePrefixKey)
	}
}

// TestListOrdersByRecency — the picker shows the most recent conversation
// first, because that is the one an operator is most likely to want.
func TestListOrdersByRecency(t *testing.T) {
	r, clock := newTestRegistry(t, Options{})

	r.Observe(obs("oldest"))
	clock.advance(time.Minute)
	r.Observe(obs("middle"))
	clock.advance(time.Minute)
	r.Observe(obs("newest"))

	got := r.List()
	want := []string{"newest", "middle", "oldest"}
	if len(got) != len(want) {
		t.Fatalf("List returned %d records, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("List()[%d].ID = %q, want %q", i, got[i].ID, id)
		}
	}
}

// TestListReturnsCopies — a caller holding a snapshot must not see later
// mutations, the classic bug in returning pointers from a guarded map.
func TestListReturnsCopies(t *testing.T) {
	r, _ := newTestRegistry(t, Options{})
	r.Observe(obs("a3f9"))

	snap := r.List()
	r.Observe(obs("a3f9"))

	if snap[0].Turns != 1 {
		t.Errorf("snapshot mutated after the fact: Turns = %d, want 1", snap[0].Turns)
	}
}

// TestIdleRecordsExpire — a conversation nobody has touched in hours should
// stop cluttering the list.
func TestIdleRecordsExpire(t *testing.T) {
	r, clock := newTestRegistry(t, Options{IdleTTL: time.Hour})

	r.Observe(obs("stale"))
	clock.advance(2 * time.Hour)
	r.Observe(obs("fresh")) // triggers eviction

	if _, ok := r.Get("stale"); ok {
		t.Error("an idle conversation was not expired")
	}
	if _, ok := r.Get("fresh"); !ok {
		t.Error("the active conversation was expired")
	}
}

// TestBoundedByMaxRecords is the memory guard. The registry is keyed by
// user-influenced input, so unbounded growth is how this becomes a leak.
func TestBoundedByMaxRecords(t *testing.T) {
	r, clock := newTestRegistry(t, Options{MaxRecords: 10})

	for i := 0; i < 50; i++ {
		r.Observe(obs(fmt.Sprintf("conv-%02d", i)))
		clock.advance(time.Second)
	}

	if got := r.Len(); got != 10 {
		t.Fatalf("Len = %d, want 10 — the bound did not hold", got)
	}
	// The survivors must be the most recent, not an arbitrary ten.
	if _, ok := r.Get("conv-49"); !ok {
		t.Error("the most recent conversation was evicted")
	}
	if _, ok := r.Get("conv-00"); ok {
		t.Error("the stalest conversation survived eviction")
	}
}

// TestConcurrentObserve runs under -race. Observe is called once per request,
// so concurrent turns across conversations are the normal case.
func TestConcurrentObserve(t *testing.T) {
	r, _ := newTestRegistry(t, Options{})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.Observe(obs(fmt.Sprintf("conv-%d", i%5)))
			_ = r.List()
			_, _ = r.Get("conv-0")
		}(i)
	}
	wg.Wait()

	if got := r.Len(); got != 5 {
		t.Errorf("Len = %d, want 5", got)
	}
	total := 0
	for _, rec := range r.List() {
		total += rec.Turns
	}
	if total != 50 {
		t.Errorf("total turns = %d, want 50 — an update was lost", total)
	}
}

// TestNilRegistryIsInert — the proxy builds a registry only when it is enabled,
// so every method must tolerate a nil receiver rather than forcing nil checks
// onto the request path.
func TestNilRegistryIsInert(t *testing.T) {
	var r *Registry
	r.Observe(obs("a3f9"))
	if r.List() != nil || r.Len() != 0 {
		t.Error("nil registry returned data")
	}
	if _, ok := r.Get("a3f9"); ok {
		t.Error("nil registry returned a record")
	}
	r.Close()
}

// TestCloseIsIdempotent — Shutdown paths can double-close.
func TestCloseIsIdempotent(t *testing.T) {
	r, _ := newTestRegistry(t, Options{})
	r.Close()
	r.Close()
}
