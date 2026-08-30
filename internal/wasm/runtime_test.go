package wasm

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// timeoutPluginWasm is a minimal plugin guest whose run_hook loops forever.
//
// Built programmatically rather than as a hex blob: the previous literal had
// to be re-hand-assembled whenever an export or signature changed, and section
// lengths silently disagreeing with their contents produces a module that
// fails to instantiate for reasons unrelated to the test.
//
// It is intentionally tiny so cancellation coverage never depends on a
// compiler being installed in CI.
var timeoutPluginWasm = MinimalModule(true)

func TestCallRequestTimeoutDiscardsInstance(t *testing.T) {
	r := NewRuntimeWithOptions(context.Background(), RuntimeOptions{
		PoolSize:    1,
		CallTimeout: 15 * time.Millisecond,
	})
	defer r.Close()

	p, err := r.LoadPlugin("timeout", timeoutPluginWasm)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	started := time.Now()
	var output []byte
	err = p.CallRequest(context.Background(), pb.Hook_HOOK_BEFORE_REQUEST, 1, []byte("x"), &output)
	if err == nil {
		t.Fatal("expected timed-out guest call to fail")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("guest call ignored deadline for %s", elapsed)
	}
	if got := len(p.pool); got != 0 {
		t.Fatalf("timed-out instance was returned to pool (%d retained)", got)
	}
}

func TestRuntimeOptionsAreNormalized(t *testing.T) {
	got := normalizeRuntimeOptions(RuntimeOptions{
		PoolSize:         -1,
		CallTimeout:      -1,
		MemoryLimitPages: 70000,
	})
	if got.PoolSize != defaultPoolSize || got.CallTimeout != defaultCallTimeout {
		t.Fatalf("defaults = %+v", got)
	}
	if got.MemoryLimitPages != 65536 {
		t.Fatalf("memory pages = %d, want WebAssembly maximum", got.MemoryLimitPages)
	}
	if got.InstanceIdleTimeout != defaultInstanceIdleTimeout {
		t.Fatalf("idle timeout = %s, want %s", got.InstanceIdleTimeout, defaultInstanceIdleTimeout)
	}
	if got := normalizeRuntimeOptions(RuntimeOptions{InstanceIdleTimeout: -1}); got.InstanceIdleTimeout != 0 {
		t.Fatalf("disabled idle timeout = %s, want zero", got.InstanceIdleTimeout)
	}
	if got := normalizeRuntimeOptions(RuntimeOptions{InstanceIdleTimeout: 17 * time.Second}); got.InstanceIdleTimeout != 17*time.Second {
		t.Fatalf("custom idle timeout = %s", got.InstanceIdleTimeout)
	}
}

func TestRetireIdleInstancesKeepsOneReady(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var closed []string
	p := &Plugin{
		pool:        make(chan *pluginInstance, 4),
		slots:       make(chan struct{}, 4),
		idleTimeout: time.Minute,
	}
	for i, age := range []time.Duration{4 * time.Minute, 3 * time.Minute, 2 * time.Minute, 90 * time.Second} {
		p.pool <- &pluginInstance{
			mod:       &fakeModule{order: &closed, tag: fmt.Sprintf("i%d", i)},
			idleSince: now.Add(-age),
		}
	}

	p.retireIdleInstances(now)
	if got := len(p.pool); got != 1 {
		t.Fatalf("retained idle instances = %d, want one", got)
	}
	kept := <-p.pool
	if got := kept.mod.(*fakeModule).tag; got != "i3" {
		t.Fatalf("retained %s, want newest i3", got)
	}
	if got, want := closed, []string{"instance:i0", "instance:i1", "instance:i2"}; !slices.Equal(got, want) {
		t.Fatalf("closed = %v, want %v", got, want)
	}
}

func TestRetireIdleInstancesPreservesRecentAndActive(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var closed []string
	p := &Plugin{
		pool:        make(chan *pluginInstance, 4),
		slots:       make(chan struct{}, 4),
		idleTimeout: time.Minute,
	}
	stale := &pluginInstance{mod: &fakeModule{order: &closed, tag: "stale"}, idleSince: now.Add(-2 * time.Minute)}
	recentA := &pluginInstance{mod: &fakeModule{order: &closed, tag: "recent-a"}, idleSince: now.Add(-30 * time.Second)}
	recentB := &pluginInstance{mod: &fakeModule{order: &closed, tag: "recent-b"}, idleSince: now.Add(-20 * time.Second)}
	active := &pluginInstance{mod: &fakeModule{order: &closed, tag: "active"}}
	p.pool <- stale
	p.pool <- recentA
	p.pool <- recentB
	p.slots <- struct{}{} // active holds one admitted-call slot and is not idle.

	p.retireIdleInstances(now)
	if got := len(p.pool); got != 2 {
		t.Fatalf("retained idle instances = %d, want two recent instances", got)
	}
	for i, want := range []string{"recent-a", "recent-b"} {
		if got := (<-p.pool).mod.(*fakeModule).tag; got != want {
			t.Fatalf("retained[%d] = %s, want %s", i, got, want)
		}
	}
	if got, want := closed, []string{"instance:stale"}; !slices.Equal(got, want) {
		t.Fatalf("closed = %v, want %v", got, want)
	}
	p.release(active)
	if slices.Contains(closed, "instance:active") {
		t.Fatal("an active instance was closed")
	}
}

func TestRetireIdleInstancesDisabledAndClosedAreNoOps(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		closed  bool
	}{
		{name: "disabled", timeout: 0},
		{name: "plugin closed", timeout: time.Second, closed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var closes []string
			p := &Plugin{
				pool:        make(chan *pluginInstance, 2),
				slots:       make(chan struct{}, 2),
				idleTimeout: tc.timeout,
				poolClosed:  tc.closed,
			}
			p.pool <- &pluginInstance{mod: &fakeModule{order: &closes, tag: "a"}, idleSince: time.Unix(1, 0)}
			p.pool <- &pluginInstance{mod: &fakeModule{order: &closes, tag: "b"}, idleSince: time.Unix(2, 0)}
			p.retireIdleInstances(time.Unix(100, 0))
			if len(p.pool) != 2 || len(closes) != 0 {
				t.Fatalf("pool=%d closes=%v, want untouched", len(p.pool), closes)
			}
		})
	}
}

func TestSetGrantsQuiescesCallsAndIdleSweep(t *testing.T) {
	var closed []string
	p := &Plugin{
		pool:  make(chan *pluginInstance, 1),
		slots: make(chan struct{}, 1),
	}
	p.pool <- &pluginInstance{mod: &fakeModule{order: &closed, tag: "old-policy"}}
	p.callMu.RLock() // model an admitted call or in-progress idle sweep
	done := make(chan struct{})
	go func() {
		p.SetGrants([]string{"env.log"})
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("SetGrants crossed the call/sweep quiescence boundary")
	case <-time.After(20 * time.Millisecond):
	}
	p.callMu.RUnlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SetGrants did not finish after quiescence")
	}
	if got := len(p.pool); got != 0 {
		t.Fatalf("old-policy pool = %d, want drained", got)
	}
	if got, want := closed, []string{"instance:old-policy"}; !slices.Equal(got, want) {
		t.Fatalf("closed = %v, want %v", got, want)
	}
	if !p.hasGrant("env.log") {
		t.Fatal("new grant set was not installed")
	}
}

func TestRuntimeRetiresBurstAndRegrowsOnDemand(t *testing.T) {
	r := NewRuntimeWithOptions(context.Background(), RuntimeOptions{
		PoolSize:            4,
		CallTimeout:         time.Second,
		InstanceIdleTimeout: 20 * time.Millisecond,
	})
	p, err := r.LoadPlugin("retirement", MinimalModule(false))
	if err != nil {
		t.Fatal(err)
	}
	instances := make([]*pluginInstance, 0, 4)
	for range 4 {
		inst, err := p.acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		instances = append(instances, inst)
	}
	for _, inst := range instances {
		p.release(inst)
	}
	deadline := time.Now().Add(time.Second)
	for len(p.pool) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(p.pool); got != 1 {
		t.Fatalf("idle pool = %d after timeout, want one", got)
	}

	regrown := acquireInstancesConcurrently(t, p, 4)
	for _, inst := range regrown {
		p.release(inst)
	}
	if got := len(p.pool); got != 4 {
		t.Fatalf("regrown pool = %d, want four", got)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if r.idleStop != nil || r.idleDone != nil {
		t.Fatal("idle-retirement worker survived runtime close")
	}
}

func TestIdleRetirementConcurrentTraffic(t *testing.T) {
	r := NewRuntimeWithOptions(context.Background(), RuntimeOptions{
		PoolSize:            4,
		CallTimeout:         time.Second,
		InstanceIdleTimeout: 2 * time.Millisecond,
	})
	defer r.Close()
	p, err := r.LoadPlugin("retirement-stress", MinimalModule(false))
	if err != nil {
		t.Fatal(err)
	}
	p.SetGrants(nil)
	start := make(chan struct{})
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 100 {
				var output []byte
				if err := p.CallRequest(context.Background(), pb.Hook_HOOK_BEFORE_REQUEST, 1, nil, &output); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent call: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for len(p.pool) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(p.pool); got != 1 {
		t.Fatalf("idle pool = %d after concurrent traffic, want one", got)
	}
}

func TestIdleRetirementStopsOnParentContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := NewRuntimeWithOptions(ctx, RuntimeOptions{InstanceIdleTimeout: time.Second})
	done := r.idleDone
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle-retirement worker ignored parent cancellation")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPoolSizeBoundsConcurrentInstances(t *testing.T) {
	r := NewRuntimeWithOptions(context.Background(), RuntimeOptions{
		PoolSize:    1,
		CallTimeout: time.Second,
	})
	defer r.Close()

	p, err := r.LoadPlugin("bounded", timeoutPluginWasm)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	first, err := p.acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.acquire(waitCtx); err == nil {
		t.Fatal("second acquire exceeded PoolSize while first instance was active")
	}
	p.release(first)

	second, err := p.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	p.release(second)
}

// TestMetaRequestScoping: meta state is isolated per request ID.
func TestMetaRequestScoping(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()

	r.metaSet(1, "tool:0", "call_a")
	r.metaSet(2, "tool:0", "call_b")

	if got := r.metaGet(1, "tool:0"); got != "call_a" {
		t.Fatalf("req 1: got %q want call_a", got)
	}
	if got := r.metaGet(2, "tool:0"); got != "call_b" {
		t.Fatalf("req 2: got %q want call_b", got)
	}
	if got := r.metaGet(3, "tool:0"); got != "" {
		t.Fatalf("req 3: got %q want empty", got)
	}
}

// TestMetaEmptyValueDeletes: setting an empty value removes the key
// (the cleanup convention plugins rely on).
// An empty meta value is a VALUE, not a delete.
//
// This test asserted the opposite, which was correct for v1. Under that rule a
// plugin could not store an empty string and could not tell "nothing stored"
// from "I stored nothing" — the ambiguity current ABI removes, and the reason absence is
// now reported as NOT_FOUND.
func TestMetaEmptyValueIsStoredNotDeleted(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()

	r.metaSet(1, "frag:x", "data")
	r.metaSet(1, "frag:x", "")

	got, present := r.metaGetPresence(1, "frag:x")
	if !present {
		t.Fatal("an empty value deleted the key; empty is a value, not a delete")
	}
	if got != "" {
		t.Fatalf("got %q, want an empty value", got)
	}

	// A key never written is absent, which is a different answer.
	if _, present := r.metaGetPresence(1, "frag:never"); present {
		t.Fatal("an unwritten key reported present")
	}
}

// TestEndRequestDropsState: EndRequest frees the whole request bucket.
func TestEndRequestDropsState(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()

	r.metaSet(7, "tool:0", "call_x")
	r.metaSet(7, "frag:call_x", "{...}")
	r.EndRequest(7)

	if got := r.metaGet(7, "tool:0"); got != "" {
		t.Fatalf("got %q want empty after EndRequest", got)
	}
	r.metaMu.RLock()
	_, exists := r.meta[7]
	r.metaMu.RUnlock()
	if exists {
		t.Fatal("request bucket still present after EndRequest")
	}
}

// TestMetaConcurrency: concurrent requests hammering meta state stay
// isolated (run with -race).
func TestMetaConcurrency(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()

	var wg sync.WaitGroup
	for reqID := uint64(1); reqID <= 20; reqID++ {
		wg.Add(1)
		go func(id uint64) {
			defer wg.Done()
			want := fmt.Sprintf("call_%d", id)
			for i := 0; i < 100; i++ {
				r.metaSet(id, "tool:0", want)
				if got := r.metaGet(id, "tool:0"); got != want {
					t.Errorf("req %d: got %q want %q", id, got, want)
					return
				}
			}
			r.EndRequest(id)
		}(reqID)
	}
	wg.Wait()
}
