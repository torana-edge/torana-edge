package wasm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func loadFixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixturesDir, name, "plugin.wasm"))
	if err != nil {
		t.Fatalf("fixture %s not built — run make testdata: %v", name, err)
	}
	return b
}

func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	return NewRuntime(context.Background())
}

func closeCounts(r *Runtime) map[string]int {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	out := make(map[string]int, len(r.compiledCloses))
	for k, v := range r.compiledCloses {
		out[k] = v
	}
	return out
}

// TestLifecycleCloseReleasesCompiledExactlyOnce — rows 1/2: every acquired
// compiled handle is released exactly once; repeated Close is a deterministic
// no-op that never increments release counts.
func TestLifecycleCloseReleasesCompiledExactlyOnce(t *testing.T) {
	r := newTestRuntime(t)
	if _, err := r.LoadPlugin("inert-a", loadFixtureBytes(t, "test-inert-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LoadPlugin("observer", loadFixtureBytes(t, "test-observer")); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	got := closeCounts(r)
	if got["inert-a"] != 1 || got["observer"] != 1 {
		t.Fatalf("compiled close counts = %v, want 1 for both", got)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	got = closeCounts(r)
	if got["inert-a"] != 1 || got["observer"] != 1 {
		t.Fatalf("repeated Close incremented release counts: %v", got)
	}
}

// TestLifecycleConcurrentCloseIsExactlyOnce — concurrent Close calls return
// the same cached result and release nothing twice.
func TestLifecycleConcurrentCloseIsExactlyOnce(t *testing.T) {
	r := newTestRuntime(t)
	if _, err := r.LoadPlugin("inert-a", loadFixtureBytes(t, "test-inert-a")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = r.Close()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Close %d: %v", i, err)
		}
	}
	if got := closeCounts(r)["inert-a"]; got != 1 {
		t.Fatalf("concurrent Close released %d times, want 1", got)
	}
}

// TestLifecycleLoadAfterCloseFails — rows 3/10: LoadPlugin and UnloadPlugin
// after Close fail/no-op deterministically and never acquire resources.
func TestLifecycleLoadAfterCloseFails(t *testing.T) {
	r := newTestRuntime(t)
	if _, err := r.LoadPlugin("inert-a", loadFixtureBytes(t, "test-inert-a")); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LoadPlugin("observer", loadFixtureBytes(t, "test-observer")); err == nil {
		t.Fatal("LoadPlugin succeeded after Close")
	} else if !strings.Contains(err.Error(), "runtime is closed") {
		t.Fatalf("unexpected post-close load error: %v", err)
	}
	if err := r.UnloadPlugin("inert-a"); err != nil {
		t.Fatalf("post-close UnloadPlugin errored: %v", err)
	}
	if got := closeCounts(r)["observer"]; got != 0 {
		t.Fatalf("post-close load compiled anything: count=%d", got)
	}
}

// TestLifecycleLoadVsCloseRace — rows 3/13: a load either publishes while the
// runtime is still open (and is then included in close) or fails as closed;
// it never publishes after the close snapshot, and no published plugin exists
// after closed. Run under -race.
func TestLifecycleLoadVsCloseRace(t *testing.T) {
	const names = 6
	r := newTestRuntime(t)
	bytes := loadFixtureBytes(t, "test-inert-a")

	var wg sync.WaitGroup
	var mu sync.Mutex
	loaded := map[string]bool{}
	for i := 0; i < names; i++ {
		name := fmt.Sprintf("race-%d", i)
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			p, err := r.LoadPlugin(name, bytes)
			if err == nil && p != nil {
				mu.Lock()
				loaded[name] = true
				mu.Unlock()
			}
		}(name)
	}
	// Start close while loads are still racing.
	if err := r.Close(); err != nil {
		t.Fatalf("Close during loads: %v", err)
	}
	wg.Wait()

	r.mu.RLock()
	published := len(r.plugins)
	r.mu.RUnlock()
	if published != 0 {
		t.Fatalf("%d plugins remain published after Close", published)
	}
	mu.Lock()
	defer mu.Unlock()
	for name := range loaded {
		if closeCounts(r)[name] != 1 {
			t.Fatalf("a published plugin %s was released %d times, want exactly 1", name, closeCounts(r)[name])
		}
	}
}

// TestLifecycleDuplicateLoadRejectedBeforeCompile — rows 4: a second load of
// the same name is a caller error, rejected BEFORE compilation, so nothing is
// ever acquired for it.
func TestLifecycleDuplicateLoadRejectedBeforeCompile(t *testing.T) {
	r := newTestRuntime(t)
	if _, err := r.LoadPlugin("inert-a", loadFixtureBytes(t, "test-inert-a")); err != nil {
		t.Fatal(err)
	}
	_, err := r.LoadPlugin("inert-a", loadFixtureBytes(t, "test-inert-a"))
	if err == nil || !strings.Contains(err.Error(), "already loaded") {
		t.Fatalf("duplicate load error = %v, want already-loaded", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if got := closeCounts(r)["inert-a"]; got != 1 {
		t.Fatalf("duplicate load compiled or released extra times: count=%d", got)
	}
}

// TestLifecycleConcurrentDuplicateLoads — at most one of N concurrent loads
// publishes; the losers fail as duplicates without compiling.
func TestLifecycleConcurrentDuplicateLoads(t *testing.T) {
	r := newTestRuntime(t)
	bytes := loadFixtureBytes(t, "test-inert-a")
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	dupErrors := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.LoadPlugin("dup", bytes)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes++
			} else if strings.Contains(err.Error(), "already loaded") {
				dupErrors++
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("%d concurrent duplicate loads succeeded, want exactly 1", successes)
	}
	if dupErrors != 7 {
		t.Fatalf("%d losers got a non-duplicate error, want 7 already-loaded", dupErrors)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if got := closeCounts(r)["dup"]; got != 1 {
		t.Fatalf("concurrent duplicates released %d times, want 1", got)
	}
}

// TestLifecycleConstructionFailureReleasesCompiled — row 5: an instance
// creation failure after a successful compile releases the compiled handle.
// A 1-page memory limit compiles fine but fails instantiation.
func TestLifecycleConstructionFailureReleasesCompiled(t *testing.T) {
	// A limit that is large enough to COMPILE the module but too small for
	// the guest's declared memory fails at INSTANTIATION (newInstance), the
	// post-compile error path that must release the acquired handle.
	// (A limit below the module's declared memory fails at compile time,
	// where nothing was acquired and the count stays 0 — also correct.)
	r := NewRuntimeWithOptions(context.Background(), RuntimeOptions{
		PoolSize:         1,
		MemoryLimitPages: 16, // below the Go guest's declared minimum
	})
	defer r.Close()
	_, err := r.LoadPlugin("tiny-mem", loadFixtureBytes(t, "test-inert-a"))
	if err == nil {
		t.Fatal("load under a restrictive memory limit unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "compile") && closeCounts(r)["tiny-mem"] != 1 {
		t.Fatalf("instantiation failure released compiled %d times, want 1 (err: %v)", closeCounts(r)["tiny-mem"], err)
	}
}

// fakeCompiled is a close-counting/error-injecting wazero.CompiledModule.
type fakeCompiled struct {
	wazero.CompiledModule
	err   error
	order *[]string
	tag   string
}

func (f *fakeCompiled) Close(context.Context) error {
	if f.order != nil {
		*f.order = append(*f.order, "compiled:"+f.tag)
	}
	return f.err
}

// fakeModule is a close-counting api.Module.
type fakeModule struct {
	api.Module
	order *[]string
	tag   string
}

func (f *fakeModule) Close(context.Context) error {
	if f.order != nil {
		*f.order = append(*f.order, "instance:"+f.tag)
	}
	return nil
}

func (f *fakeModule) IsClosed() bool { return false }

// TestLifecycleCleanupErrorsJoinedAndSorted — row 8: one failed close does not
// block the others, and the joined error is deterministic (sorted names).
func TestLifecycleCleanupErrorsJoinedAndSorted(t *testing.T) {
	r := newTestRuntime(t)
	var order []string
	r.mu.Lock()
	r.plugins["b-failing"] = &Plugin{
		name:     "b-failing",
		compiled: &fakeCompiled{err: errors.New("boom"), tag: "b"},
		pool:     make(chan *pluginInstance, 1),
		slots:    make(chan struct{}, 1),
	}
	r.plugins["a-good"] = &Plugin{
		name:     "a-good",
		compiled: &fakeCompiled{tag: "a"},
		pool:     make(chan *pluginInstance, 1),
		slots:    make(chan struct{}, 1),
	}
	r.mu.Unlock()

	err := r.Close()
	if err == nil {
		t.Fatal("Close with a failing plugin returned nil")
	}
	msg := err.Error()
	// Sorted order: a-good's close attempt first.
	if strings.Index(msg, "a-good") > strings.Index(msg, "b-failing") {
		t.Fatalf("joined error is not sorted: %s", msg)
	}
	_ = order
	if got := closeCounts(r)["a-good"]; got != 1 {
		t.Fatalf("a-good released %d times, want 1 (one failure must not block others)", got)
	}
	if got := closeCounts(r)["b-failing"]; got != 1 {
		t.Fatalf("b-failing released %d times, want 1", got)
	}
}

// TestLifecycleInstancesClosedBeforeCompiled — row 9: instances are closed
// before the compiled handle, without relying on wazero's documented
// compiled-close-with-open-instances safety.
func TestLifecycleInstancesClosedBeforeCompiled(t *testing.T) {
	r := newTestRuntime(t)
	var order []string
	p := &Plugin{
		name:     "ordered",
		compiled: &fakeCompiled{order: &order, tag: "c"},
		pool:     make(chan *pluginInstance, 1),
		slots:    make(chan struct{}, 1),
	}
	p.pool <- &pluginInstance{mod: &fakeModule{order: &order, tag: "i"}}
	r.mu.Lock()
	r.plugins["ordered"] = p
	r.mu.Unlock()

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "instance:i" || order[1] != "compiled:c" {
		t.Fatalf("close order = %v, want instance before compiled", order)
	}
}

// TestLifecycleActiveInstanceNotReturnedToPool — row 14: an instance checked
// out while the runtime closes is CLOSED on return (never re-queued into the
// dead pool), and post-close acquire fails deterministically.
func TestLifecycleActiveInstanceNotReturnedToPool(t *testing.T) {
	r := NewRuntimeWithOptions(context.Background(), RuntimeOptions{PoolSize: 2})
	defer r.Close()
	p, err := r.LoadPlugin("inert-a", loadFixtureBytes(t, "test-inert-a"))
	if err != nil {
		t.Fatal(err)
	}
	// Take the pre-warmed instance out of the pool; acquire creates a second
	// one. The second stays "in flight" while we close.
	held := <-p.pool
	inst2, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// The in-flight (slot-tracked) instance's return must close it, not
	// re-queue it. The pre-warmed instance is not slot-tracked (LoadPlugin
	// seeds the pool directly), so close its module directly.
	p.release(inst2)
	if !inst2.mod.IsClosed() {
		t.Fatal("the in-flight instance was returned to the dead pool instead of closed")
	}
	if held != nil && !held.mod.IsClosed() {
		if err := held.mod.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if !inst2.mod.IsClosed() {
		t.Fatal("the in-flight instance was returned to the dead pool instead of closed")
	}
	if len(p.pool) != 0 {
		t.Fatalf("%d instances remain in the closed pool", len(p.pool))
	}
	if _, err := p.acquire(context.Background()); err == nil {
		t.Fatal("acquire succeeded on a closed plugin")
	}
	if got := closeCounts(r)["inert-a"]; got != 1 {
		t.Fatalf("compiled released %d times, want exactly 1", got)
	}
}

// TestLifecycleActiveCallVsClose — row 14: a guest call racing Runtime.Close
// must not panic, and the compiled handle is released exactly once.
func TestLifecycleActiveCallVsClose(t *testing.T) {
	r := newTestRuntime(t)
	bytes := loadFixtureBytes(t, "test-inert-a")
	p, err := r.LoadPlugin("inert-a", bytes)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A real hook dispatch racing close; any outcome is acceptable except
		// a panic, which the test harness turns into a failure.
		_ = p.CallRequest(context.Background(), pb.Hook_HOOK_BEFORE_REQUEST, 1, nil, nil)
	}()
	if err := r.Close(); err != nil {
		t.Fatalf("Close during an active call: %v", err)
	}
	<-done
	if got := closeCounts(r)["inert-a"]; got != 1 {
		t.Fatalf("compiled released %d times during an active call, want 1", got)
	}
}

// TestLifecycleNoPublishedPluginsAfterClose — row 13 invariant.
func TestLifecycleNoPublishedPluginsAfterClose(t *testing.T) {
	r := newTestRuntime(t)
	if _, err := r.LoadPlugin("inert-a", loadFixtureBytes(t, "test-inert-a")); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r.mu.RLock()
	n := len(r.plugins)
	r.mu.RUnlock()
	if n != 0 {
		t.Fatalf("%d plugins published after closed", n)
	}
}

// TestLifecycleSortedNamesDeterministic — repeated Close yields the same
// joined error order across runs (sorted names).
func TestLifecycleSortedNamesDeterministic(t *testing.T) {
	var msgs []string
	for i := 0; i < 3; i++ {
		r := newTestRuntime(t)
		var order []string
		_ = order
		r.mu.Lock()
		for _, n := range []string{"zeta", "alpha", "mid"} {
			r.plugins[n] = &Plugin{
				name:     n,
				compiled: &fakeCompiled{err: errors.New("x"), tag: n},
				pool:     make(chan *pluginInstance, 1),
				slots:    make(chan struct{}, 1),
			}
		}
		r.mu.Unlock()
		msgs = append(msgs, r.Close().Error())
	}
	if msgs[0] != msgs[1] || msgs[1] != msgs[2] {
		t.Fatalf("joined close error is not deterministic across runs: %v", msgs)
	}
	if !strings.Contains(msgs[0], "alpha:") || !strings.Contains(msgs[0], "zeta:") {
		t.Fatalf("unexpected joined error: %s", msgs[0])
	}
	if strings.Index(msgs[0], "alpha:") > strings.Index(msgs[0], "mid:") || strings.Index(msgs[0], "mid:") > strings.Index(msgs[0], "zeta:") {
		t.Fatalf("joined error not in sorted order: %s", msgs[0])
	}
}

// TestLifecycleUnloadExactlyOnceAndRemovesReachability — UnloadPlugin removes
// reachability before releasing, releases exactly once, and a second unload
// is a no-op.
func TestLifecycleUnloadExactlyOnceAndRemovesReachability(t *testing.T) {
	r := newTestRuntime(t)
	if _, err := r.LoadPlugin("inert-a", loadFixtureBytes(t, "test-inert-a")); err != nil {
		t.Fatal(err)
	}
	if err := r.UnloadPlugin("inert-a"); err != nil {
		t.Fatal(err)
	}
	r.mu.RLock()
	_, present := r.plugins["inert-a"]
	r.mu.RUnlock()
	if present {
		t.Fatal("unloaded plugin still reachable")
	}
	if err := r.UnloadPlugin("inert-a"); err != nil {
		t.Fatalf("second unload errored: %v", err)
	}
	if got := closeCounts(r)["inert-a"]; got != 1 {
		t.Fatalf("unload released compiled %d times, want exactly 1", got)
	}
	// The name is free again.
	if _, err := r.LoadPlugin("inert-a", loadFixtureBytes(t, "test-inert-a")); err != nil {
		t.Fatalf("reload after unload failed: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if got := closeCounts(r)["inert-a"]; got != 2 {
		t.Fatalf("two loads/unloads released %d times, want 2", got)
	}
}

// TestLifecycleFakeCompiledCloseErrorRecorded — the seam counts even when
// Close fails (the caller still attempted the release).
func TestLifecycleFakeCompiledCloseErrorRecorded(t *testing.T) {
	r := newTestRuntime(t)
	r.mu.Lock()
	r.plugins["bad"] = &Plugin{
		name:     "bad",
		compiled: &fakeCompiled{err: errors.New("boom")},
		pool:     make(chan *pluginInstance, 1),
		slots:    make(chan struct{}, 1),
	}
	r.mu.Unlock()
	if err := r.Close(); err == nil {
		t.Fatal("expected the injected close error")
	}
	if got := closeCounts(r)["bad"]; got != 1 {
		t.Fatalf("failed close recorded %d releases, want 1", got)
	}
}
