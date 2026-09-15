package proxy

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
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

func TestStreamingFinalHookSurvivesRequestCancellation(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-observer/plugin.wasm")
	srv, err := New(Config{Providers: provider.Config{Plugins: provider.PluginsConfig{
		Dir: fixturesDir, Order: []string{"test-observer"}, AllowUnapproved: true,
	}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	pl, ok := srv.pluginPipeline.Load().(*plugin.PluginPipeline)
	if !ok || pl == nil {
		t.Fatal("test-observer pipeline was not loaded")
	}

	rs := &reqState{ID: 77, Model: "gpt-x", UpstreamStatus: 200, UsageIn: 10, UsageOut: 5, UsageReported: true}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), reqStateKey{}, rs))
	cancel()
	if parent.Err() == nil {
		t.Fatal("test precondition: request context was not cancelled")
	}

	done := make(chan error, 1)
	go func() { done <- runStreamingAfterResponse(parent, pl) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("terminal hook: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal hook exceeded its bounded runtime")
	}
	v, found, cacheErr := srv.sharedCache.Get(context.Background(), observerCacheKey("observed_error_status"))
	if cacheErr != nil || !found || v != "200" {
		t.Fatalf("terminal hook lost request state or cache resource: value=%q found=%v err=%v", v, found, cacheErr)
	}
}
