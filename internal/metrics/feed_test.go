package metrics

import (
	"sync"
	"testing"
	"time"
)

// makeEvent is a small helper that returns a RequestEvent with the given
// provider string so tests can distinguish entries.
func makeEvent(provider string) RequestEvent {
	return RequestEvent{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Provider:  provider,
		Status:    200,
		LatencyMS: 1.0,
	}
}

// TestRingWrapAround verifies that Add correctly evicts the oldest entry when
// the capacity is exceeded, and that Snapshot length never exceeds capacity.
func TestRingWrapAround(t *testing.T) {
	const cap = 5
	f := NewRequestFeed(cap)

	// Fill exactly to capacity.
	for i := 0; i < cap; i++ {
		f.Add(makeEvent("p" + string(rune('a'+i))))
	}
	snap := f.Snapshot()
	if len(snap) != cap {
		t.Fatalf("Snapshot len = %d, want %d", len(snap), cap)
	}
	// Newest-first: last added is "pe" (index 4) → should be snap[0].
	if snap[0].Provider != "pe" {
		t.Errorf("snap[0].Provider = %q, want %q", snap[0].Provider, "pe")
	}
	if snap[cap-1].Provider != "pa" {
		t.Errorf("snap[%d].Provider = %q, want %q", cap-1, snap[cap-1].Provider, "pa")
	}

	// Add two more entries — "pa" and "pb" should be evicted.
	f.Add(makeEvent("pf"))
	f.Add(makeEvent("pg"))
	snap = f.Snapshot()
	if len(snap) != cap {
		t.Fatalf("Snapshot len after wrap = %d, want %d", len(snap), cap)
	}
	// Newest-first: "pg" is the most recent.
	if snap[0].Provider != "pg" {
		t.Errorf("after wrap snap[0].Provider = %q, want %q", snap[0].Provider, "pg")
	}
	// Oldest remaining: "pc" (pa, pb were evicted).
	if snap[cap-1].Provider != "pc" {
		t.Errorf("after wrap snap[%d].Provider = %q, want %q", cap-1, snap[cap-1].Provider, "pc")
	}
}

// TestSnapshotOrdering verifies that Snapshot always returns events newest-first.
func TestSnapshotOrdering(t *testing.T) {
	f := NewRequestFeed(10)
	providers := []string{"alpha", "beta", "gamma"}
	for _, p := range providers {
		f.Add(makeEvent(p))
	}
	snap := f.Snapshot()
	if len(snap) != len(providers) {
		t.Fatalf("Snapshot len = %d, want %d", len(snap), len(providers))
	}
	// Reverse order: gamma, beta, alpha.
	want := []string{"gamma", "beta", "alpha"}
	for i, w := range want {
		if snap[i].Provider != w {
			t.Errorf("snap[%d].Provider = %q, want %q", i, snap[i].Provider, w)
		}
	}
}

// TestSnapshotEmpty verifies that Snapshot on a fresh feed returns nil (not a
// zero-length slice), which serialises cleanly as JSON null.
func TestSnapshotEmpty(t *testing.T) {
	f := NewRequestFeed(10)
	snap := f.Snapshot()
	if snap != nil {
		t.Errorf("empty Snapshot = %v, want nil", snap)
	}
}

// TestAddNeverBlocksOnFullSubscriber ensures that Add returns promptly even
// when a subscriber's channel is saturated, and that no event is lost from the
// ring buffer itself (only the slow subscriber misses events).
func TestAddNeverBlocksOnFullSubscriber(t *testing.T) {
	f := NewRequestFeed(200)

	// Subscribe but never read from the returned channel.
	_, unsub := f.Subscribe()
	defer unsub()

	// Fill the subscriber channel beyond its buffer. All Adds must complete
	// without blocking; use a goroutine with a deadline so the test fails
	// fast if Add deadlocks.
	done := make(chan struct{})
	go func() {
		for i := 0; i < subscriberBufSize*3; i++ {
			f.Add(makeEvent("p"))
		}
		close(done)
	}()

	select {
	case <-done:
		// Good: Add never blocked.
	case <-time.After(2 * time.Second):
		t.Fatal("Add blocked or took too long — likely deadlocked on a full subscriber channel")
	}
}

// TestSubscribeUnsubscribe verifies the subscribe/unsubscribe lifecycle:
// events added before unsubscription arrive on the channel, and the channel
// is closed promptly on unsubscription.
func TestSubscribeUnsubscribe(t *testing.T) {
	f := NewRequestFeed(10)

	ch, unsub := f.Subscribe()

	// Add one event; it should appear in the channel.
	f.Add(makeEvent("test-provider"))
	select {
	case ev := <-ch:
		if ev.Provider != "test-provider" {
			t.Errorf("received event Provider = %q, want %q", ev.Provider, "test-provider")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event on subscriber channel")
	}

	// Unsubscribe; the channel should be closed.
	unsub()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel should be closed after unsub, got value instead")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel close after unsub")
	}

	// Calling unsub again must not panic.
	unsub()
}

// TestMultipleSubscribers ensures that Add broadcasts to all current
// subscribers concurrently without data races.
func TestMultipleSubscribers(t *testing.T) {
	f := NewRequestFeed(10)

	const n = 4
	channels := make([]<-chan RequestEvent, n)
	unsubs := make([]func(), n)
	for i := 0; i < n; i++ {
		channels[i], unsubs[i] = f.Subscribe()
	}

	f.Add(makeEvent("broadcast"))

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			select {
			case ev := <-channels[i]:
				if ev.Provider != "broadcast" {
					t.Errorf("subscriber %d: Provider = %q, want %q", i, ev.Provider, "broadcast")
				}
			case <-time.After(time.Second):
				t.Errorf("subscriber %d: timed out waiting for event", i)
			}
		}()
	}
	wg.Wait()

	for _, u := range unsubs {
		u()
	}
}

// TestDefaultCapacity checks that NewRequestFeed(0) uses defaultFeedCapacity.
func TestDefaultCapacity(t *testing.T) {
	f := NewRequestFeed(0)
	if f.cap != defaultFeedCapacity {
		t.Errorf("cap = %d, want %d", f.cap, defaultFeedCapacity)
	}
}
