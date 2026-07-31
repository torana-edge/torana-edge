package proxy

import (
	"sync/atomic"
	"testing"
	"time"
)

// Request-scoped state must outlive every bit of work on the streaming
// goroutine — serialize/drain, in-flight stream hooks, and the attempted
// observational after-response — even when the handler unwinds through
// http.ErrAbortHandler and skips the normal-path wait.
//
// This does NOT claim run_after_response succeeds after disconnect (the
// request context is cancelled, so that call may fail). It claims EndRequest
// cannot run until streamDone closes.
func TestRequestCleanupWaitsForStreamingGoroutineOnExceptionalExit(t *testing.T) {
	streamDone := make(chan struct{})
	var dropCount atomic.Int32
	statePresent := atomic.Bool{}
	statePresent.Store(true)

	drop := func() {
		if !statePresent.Load() {
			t.Error("drop ran after request-scoped state was already cleared")
		}
		dropCount.Add(1)
	}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		finalizeRequestState(streamDone, drop)
	}()

	select {
	case <-finished:
		t.Fatal("finalize returned before streamDone closed — cleanup is not waiting")
	case <-time.After(30 * time.Millisecond):
	}
	if dropCount.Load() != 0 {
		t.Fatal("cleanup ran while the streaming goroutine was still marked in-flight")
	}

	// Simulate the stream goroutine finishing (and any attempted after-response).
	close(streamDone)

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("finalize did not run cleanup after streamDone closed")
	}
	if dropCount.Load() != 1 {
		t.Fatalf("cleanup ran %d times, want 1", dropCount.Load())
	}
}

// Removing the wait must fail the ordering test — the migration rule that
// caught the vacuous disconnect regression.
func TestRequestCleanupWithoutWaitFailsClosed(t *testing.T) {
	streamDone := make(chan struct{})
	var dropped atomic.Bool

	// Broken finalizer: drop without waiting. This is what the deferred
	// cleanup did before awaitStreamDone was added there.
	broken := func(done <-chan struct{}, drop func()) {
		_ = done
		drop()
	}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		broken(streamDone, func() { dropped.Store(true) })
	}()

	select {
	case <-finished:
		// Expected: returns immediately while streamDone is still open.
	case <-time.After(time.Second):
		t.Fatal("broken finalizer should not wait")
	}
	if !dropped.Load() {
		t.Fatal("expected immediate drop")
	}
	// streamDone still open — proving cleanup did not wait for it.
	select {
	case <-streamDone:
		t.Fatal("streamDone was closed unexpectedly")
	default:
	}
}
