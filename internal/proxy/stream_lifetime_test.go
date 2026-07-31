package proxy

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
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
	synctest.Test(t, func(t *testing.T) {
		streamDone := make(chan struct{})
		var dropCount atomic.Int32
		var finished atomic.Bool

		go func() {
			finalizeRequestState(streamDone, func() { dropCount.Add(1) })
			finished.Store(true)
		}()

		// Durably blocked on streamDone — not a wall-clock guess.
		synctest.Wait()
		if finished.Load() || dropCount.Load() != 0 {
			t.Fatal("cleanup ran while the streaming goroutine was still marked in-flight")
		}

		close(streamDone)
		synctest.Wait()
		if !finished.Load() {
			t.Fatal("finalize did not return after streamDone closed")
		}
		if dropCount.Load() != 1 {
			t.Fatalf("cleanup ran %d times, want 1", dropCount.Load())
		}
	})
}
