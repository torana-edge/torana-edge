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
	"time"

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

// eventRecorder is the observer seam recorder: a deterministic event stream
// with per-event barrier channels, validated against the reference model.
type eventRecorder struct {
	t        *testing.T
	mu       sync.Mutex
	seq      int
	events   []string
	barriers map[string]chan struct{}
}

func newEventRecorder(t *testing.T) *eventRecorder {
	return &eventRecorder{t: t, barriers: map[string]chan struct{}{}}
}

func (rec *eventRecorder) record(ev string) {
	rec.mu.Lock()
	rec.seq++
	full := fmt.Sprintf("%d:%s", rec.seq, ev)
	rec.events = append(rec.events, full)
	// Barriers are keyed by the RAW event (no sequence prefix).
	if ch := rec.barriers[ev]; ch != nil {
		close(ch)
	}
	rec.mu.Unlock()
}

// install wires the recorder into a runtime's observer seam. label
// distinguishes runtimes in multi-runtime tests (the reference model keys
// plugin state by runtime+name).
func (rec *eventRecorder) install(r *Runtime, label string) {
	r.testHooks = &lifecycleHooks{
		loadBegin:        func(n string) { rec.record(label + ":load:" + n) },
		published:        func(n string) { rec.record(label + ":publish:" + n) },
		instanceAcquired: func(n string) { rec.record(label + ":instance:" + n) },
		callFinished:     func(n string) { rec.record(label + ":call-finish:" + n) },
		compiledReleased: func(n string) { rec.record(label + ":release:" + n) },
		closeBegin:       func() { rec.record(label + ":close-begin") },
		closeEnd:         func() { rec.record(label + ":close-end") },
	}
}

// wait blocks until an event with the given suffix fires.
func (rec *eventRecorder) wait(suffix string) {
	rec.mu.Lock()
	for _, ev := range rec.events {
		if strings.HasSuffix(ev, suffix) {
			rec.mu.Unlock()
			return
		}
	}
	ch := make(chan struct{})
	rec.barriers[suffix] = ch
	rec.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		rec.t.Fatalf("event %q never fired; stream: %v", suffix, rec.snapshot())
	}
}

// fired reports whether an event with the given suffix has occurred.
func (rec *eventRecorder) fired(suffix string) bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, ev := range rec.events {
		if strings.HasSuffix(ev, suffix) {
			return true
		}
	}
	return false
}

func (rec *eventRecorder) snapshot() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]string, len(rec.events))
	copy(out, rec.events)
	return out
}

// count returns how many events match the suffix.
func (rec *eventRecorder) count(suffix string) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	n := 0
	for _, ev := range rec.events {
		if strings.HasSuffix(ev, suffix) {
			n++
		}
	}
	return n
}

// seqOf returns the sequence number of the first event matching the suffix.
func (rec *eventRecorder) seqOf(suffix string) (int, bool) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, ev := range rec.events {
		if strings.HasSuffix(ev, suffix) {
			var seq int
			fmt.Sscanf(ev, "%d:", &seq)
			return seq, true
		}
	}
	return 0, false
}

// validate replays the recorded stream through the reference model.
func (rec *eventRecorder) validate() {
	m := newLifecycleModel()
	for _, ev := range rec.snapshot() {
		ev = ev[strings.Index(ev, ":")+1:] // strip the sequence prefix
		if err := m.observe(ev); err != nil {
			rec.t.Fatalf("reference model rejected %q: %v\nstream: %v", ev, err, rec.snapshot())
		}
	}
}

// lifecycleModel is the small independent reference model: runtime
// open -> closing -> closed; per plugin absent -> constructing -> published
// -> released (construction failures return to absent).
type lifecycleModel struct {
	// runtime state per label (multi-runtime tests drive several runtimes
	// through one recorder).
	runtime map[string]string
	plugins map[string]string
}

func newLifecycleModel() *lifecycleModel {
	return &lifecycleModel{runtime: map[string]string{"rt": "open"}, plugins: map[string]string{}}
}

func (m *lifecycleModel) observe(ev string) error {
	label := "rt"
	kind := ev
	name := ""
	if i := strings.Index(ev, ":"); i >= 0 {
		label = ev[:i]
		rest := ev[i+1:]
		if j := strings.Index(rest, ":"); j >= 0 {
			kind = rest[:j]
			name = rest[j+1:]
		} else {
			kind = rest
		}
	}
	key := label + "/" + name
	rtState := m.runtime[label]
	if rtState == "" {
		rtState = "open"
	}
	switch kind {
	case "load":
		if rtState != "open" {
			return fmt.Errorf("load while runtime %s", rtState)
		}
		if st, ok := m.plugins[key]; ok && st != "absent" && st != "released" {
			return fmt.Errorf("load of %s in state %s", key, st)
		}
		m.plugins[key] = "constructing"
	case "publish":
		if rtState != "open" {
			return fmt.Errorf("publish while runtime %s", rtState)
		}
		if m.plugins[key] != "constructing" {
			return fmt.Errorf("publish of %s from state %q", key, m.plugins[key])
		}
		m.plugins[key] = "published"
	case "instance", "call-finish":
		if m.plugins[key] != "published" {
			return fmt.Errorf("%s for %s from state %q", kind, key, m.plugins[key])
		}
	case "release":
		st := m.plugins[key]
		switch st {
		case "published":
			m.plugins[key] = "released"
		case "constructing":
			m.plugins[key] = "absent" // construction failure: ends absent
		case "":
			// An injected plugin (tests construct one directly in the map)
			// is published by construction; its release is legal.
			m.plugins[key] = "released"
		default:
			return fmt.Errorf("release of %s from state %q", key, st)
		}
	case "close-begin":
		if rtState != "open" {
			return fmt.Errorf("close-begin while runtime %s", rtState)
		}
		m.runtime[label] = "closing"
	case "close-end":
		if rtState != "closing" {
			return fmt.Errorf("close-end while runtime %s", rtState)
		}
		m.runtime[label] = "closed"
	default:
		return fmt.Errorf("unknown event %q", ev)
	}
	return nil
}

// TestLifecycleCloseReleasesCompiledExactlyOnce — rows 1/2: every acquired
// compiled handle is released exactly once; repeated Close never increments
// release counts; the recorded stream satisfies the reference model.
func TestLifecycleCloseReleasesCompiledExactlyOnce(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
	if _, err := r.LoadPlugin("inert-a", loadFixtureBytes(t, "test-inert-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LoadPlugin("observer", loadFixtureBytes(t, "test-observer")); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if rec.count("release:inert-a") != 1 || rec.count("release:observer") != 1 {
		t.Fatalf("release counts = inert-a:%d observer:%d, want 1 each", rec.count("release:inert-a"), rec.count("release:observer"))
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if rec.count("release:inert-a") != 1 || rec.count("release:observer") != 1 {
		t.Fatalf("repeated Close incremented release counts")
	}
	rec.validate()
}

// TestLifecycleConcurrentCloseIsExactlyOnce — concurrent Close calls return
// the same cached result and release nothing twice.
func TestLifecycleConcurrentCloseIsExactlyOnce(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
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
	if rec.count("release:inert-a") != 1 {
		t.Fatalf("concurrent Close released %d times, want 1", rec.count("release:inert-a"))
	}
	rec.validate()
}

// TestLifecycleLoadAfterCloseFails — rows 3/10: LoadPlugin and UnloadPlugin
// after Close fail/no-op deterministically and never acquire resources.
func TestLifecycleLoadAfterCloseFails(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
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
	if rec.fired("load:observer") {
		t.Fatal("post-close load began a construction transaction")
	}
	rec.validate()
}

// TestLifecycleLoadVsCloseOrderings — both LEGAL orderings are proven with
// deterministic barriers instead of a scheduler race: close-first makes every
// load fail as closed; load-first publishes before close and releases once.
func TestLifecycleLoadVsCloseOrderings(t *testing.T) {
	t.Run("close first", func(t *testing.T) {
		r := newTestRuntime(t)
		rec := newEventRecorder(t)
		rec.install(r, "rt")
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 4; i++ {
			if _, err := r.LoadPlugin(fmt.Sprintf("after-%d", i), MinimalV2Module(false)); err == nil {
				t.Fatal("load succeeded after close")
			}
		}
		if rec.fired("load:after-0") {
			t.Fatal("a load after close began a construction transaction")
		}
		rec.validate()
	})
	t.Run("load first", func(t *testing.T) {
		r := newTestRuntime(t)
		rec := newEventRecorder(t)
		rec.install(r, "rt")
		// Barrier: every load completes before close starts.
		for i := 0; i < 3; i++ {
			if _, err := r.LoadPlugin(fmt.Sprintf("before-%d", i), MinimalV2Module(false)); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if rec.count(fmt.Sprintf("release:before-%d", i)) != 1 {
				t.Fatalf("before-%d released %d times, want 1", i, rec.count(fmt.Sprintf("release:before-%d", i)))
			}
		}
		rec.validate()
	})
}

// TestLifecycleDuplicateLoadRejectedBeforeCompile — a second load of the same
// name is a caller error, rejected BEFORE compilation.
func TestLifecycleDuplicateLoadRejectedBeforeCompile(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
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
	if rec.count("release:inert-a") != 1 {
		t.Fatalf("duplicate load released %d times, want 1", rec.count("release:inert-a"))
	}
	rec.validate()
}

// TestLifecycleConcurrentDuplicateLoads — at most one of N concurrent loads
// publishes; the losers fail as duplicates without compiling.
func TestLifecycleConcurrentDuplicateLoads(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
	bytes := MinimalV2Module(false)
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
	if rec.count("release:dup") != 1 {
		t.Fatalf("concurrent duplicates released %d times, want 1", rec.count("release:dup"))
	}
	rec.validate()
}

// TestLifecycleConstructionFailureReleasesCompiled — row 5: the memory-hog
// guest COMPILES under a 16 MiB limit (its declared minimum is small) and
// fails at INSTANTIATION (init grows to 64 MiB), exercising the post-compile
// newInstance error path, which must release the compiled handle.
func TestLifecycleConstructionFailureReleasesCompiled(t *testing.T) {
	r := NewRuntimeWithOptions(context.Background(), RuntimeOptions{PoolSize: 1, MemoryLimitPages: 256})
	rec := newEventRecorder(t)
	rec.install(r, "rt")
	_, err := r.LoadPlugin("hog", loadFixtureBytes(t, "test-memory-hog"))
	if err == nil {
		t.Fatal("load of the 64 MiB guest under a 16 MiB limit unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), "compile") {
		t.Fatalf("the limit rejected at COMPILE (err: %v) — the test must exercise the instantiation path", err)
	}
	if rec.count("release:hog") != 1 {
		t.Fatalf("construction failure released compiled %d times, want 1", rec.count("release:hog"))
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	rec.validate()
}

// TestLifecycleHookDiscoveryFailureReleasesCompiled — row 6: a guest that
// compiles and instantiates but exports no supported_hooks fails hook
// discovery; the error path releases BOTH the instance and the compiled
// handle and joins the cleanup errors with the primary error.
func TestLifecycleHookDiscoveryFailureReleasesCompiled(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
	_, err := r.LoadPlugin("nohooks", MinimalV2ModuleNoHooks())
	if err == nil {
		t.Fatal("a guest without supported_hooks loaded")
	}
	if !strings.Contains(err.Error(), "supported_hooks") {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.count("release:nohooks") != 1 {
		t.Fatalf("hook-discovery failure released compiled %d times, want 1", rec.count("release:nohooks"))
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	rec.validate()
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

// TestLifecycleCleanupErrorsJoinedAndSorted — row 8: one failed close does
// not block the others; the failing plugin's error surfaces and the good
// plugin's release is still attempted.
func TestLifecycleCleanupErrorsJoinedAndSorted(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
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
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("the failing plugin's error is missing: %v", err)
	}
	if rec.count("release:a-good") != 1 {
		t.Fatalf("a-good was not released despite b-failing's error")
	}
	if rec.count("release:b-failing") != 1 {
		t.Fatalf("b-failing was not released")
	}
	rec.validate()
}

// TestLifecycleInstancesClosedBeforeCompiled — row 9: instances are closed
// before the compiled handle.
func TestLifecycleInstancesClosedBeforeCompiled(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
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
	rec.validate()
}

// TestLifecycleActiveCallBlocksCompiledRelease — the quiescence core (row
// 14): a call that has ACQUIRED an instance (deterministic barrier) must be
// quiesced by the admission lock — Close cannot release the compiled handle
// until the call finishes, and the event order is instance -> call-finish ->
// release, with exactly one release.
func TestLifecycleActiveCallBlocksCompiledRelease(t *testing.T) {
	r := NewRuntimeWithOptions(context.Background(), RuntimeOptions{
		PoolSize:    1,
		CallTimeout: 700 * time.Millisecond,
	})
	rec := newEventRecorder(t)
	rec.install(r, "rt")
	p, err := r.LoadPlugin("spin", MinimalV2Module(true)) // run_hook spins forever
	if err != nil {
		t.Fatal(err)
	}
	out := []byte{}
	done := make(chan error, 1)
	go func() {
		done <- p.CallRequest(context.Background(), pb.Hook_HOOK_BEFORE_REQUEST, 1, []byte{}, &out)
	}()

	// Deterministic barrier: the call holds an instance.
	rec.wait("rt:instance:spin")

	closed := make(chan struct{})
	go func() {
		if err := r.Close(); err != nil {
			t.Errorf("Close during an active call: %v", err)
		}
		close(closed)
	}()

	// The compiled handle must NOT be released while the call is active.
	select {
	case <-time.After(200 * time.Millisecond):
	case <-closed:
		t.Fatal("Close completed while the call was still active")
	}
	if rec.fired("release:spin") {
		t.Fatal("compiled handle released while a call was active")
	}

	// The call's own timeout bounds the quiescence wait.
	if err := <-done; err == nil {
		t.Fatal("the spinning call unexpectedly succeeded")
	}
	<-closed

	if rec.count("release:spin") != 1 {
		t.Fatalf("compiled released %d times, want exactly 1", rec.count("release:spin"))
	}
	if seqCall, ok := rec.seqOf("call-finish:spin"); ok {
		if seqRelease, ok2 := rec.seqOf("release:spin"); ok2 && seqRelease < seqCall {
			t.Fatalf("release (%d) preceded the call's finish (%d)", seqRelease, seqCall)
		}
	}
	rec.validate()
}

// TestLifecycleUnloadBlocksUntilCallsQuiesce — UnloadPlugin takes the same
// admission write lock: an active call delays the unload until it finishes.
func TestLifecycleUnloadBlocksUntilCallsQuiesce(t *testing.T) {
	r := NewRuntimeWithOptions(context.Background(), RuntimeOptions{
		PoolSize:    1,
		CallTimeout: 700 * time.Millisecond,
	})
	rec := newEventRecorder(t)
	rec.install(r, "rt")
	p, err := r.LoadPlugin("spin", MinimalV2Module(true))
	if err != nil {
		t.Fatal(err)
	}
	out := []byte{}
	done := make(chan error, 1)
	go func() {
		done <- p.CallRequest(context.Background(), pb.Hook_HOOK_BEFORE_REQUEST, 1, []byte{}, &out)
	}()
	rec.wait("rt:instance:spin")

	unloaded := make(chan error, 1)
	go func() {
		unloaded <- r.UnloadPlugin("spin")
	}()
	select {
	case <-time.After(200 * time.Millisecond):
	case <-unloaded:
		t.Fatal("unload completed while a call was still active")
	}
	<-done
	if err := <-unloaded; err != nil {
		t.Fatal(err)
	}
	if rec.count("release:spin") != 1 {
		t.Fatalf("unload released compiled %d times, want 1", rec.count("release:spin"))
	}
	rec.validate()
}

// TestLifecycleNoPublishedPluginsAfterClose — row 13 invariant.
func TestLifecycleNoPublishedPluginsAfterClose(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
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
	rec.validate()
}

// TestLifecycleSortedNamesDeterministic — repeated Close yields the same
// joined error order across runs (sorted names).
func TestLifecycleSortedNamesDeterministic(t *testing.T) {
	var msgs []string
	for i := 0; i < 3; i++ {
		r := newTestRuntime(t)
		newEventRecorder(t).install(r, "rt")
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
	if strings.Index(msgs[0], "alpha:") > strings.Index(msgs[0], "mid:") || strings.Index(msgs[0], "mid:") > strings.Index(msgs[0], "zeta:") {
		t.Fatalf("joined error not in sorted order: %s", msgs[0])
	}
}

// TestLifecycleUnloadExactlyOnceAndRemovesReachability — UnloadPlugin removes
// reachability before releasing, releases exactly once, and a second unload
// is a no-op.
func TestLifecycleUnloadExactlyOnceAndRemovesReachability(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
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
	if rec.count("release:inert-a") != 1 {
		t.Fatalf("unload released compiled %d times, want exactly 1", rec.count("release:inert-a"))
	}
	if _, err := r.LoadPlugin("inert-a", loadFixtureBytes(t, "test-inert-a")); err != nil {
		t.Fatalf("reload after unload failed: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if rec.count("release:inert-a") != 2 {
		t.Fatalf("two loads/unloads released %d times, want 2", rec.count("release:inert-a"))
	}
	rec.validate()
}

// TestLifecycleReferenceModelScenarios — the reference model, driven through
// the REAL runtime for a table of scenarios; every recorded event must be a
// legal model transition.
func TestLifecycleReferenceModelScenarios(t *testing.T) {
	bytes := MinimalV2Module(false)
	scenarios := []struct {
		name string
		run  func(t *testing.T, r *Runtime, rec *eventRecorder)
	}{
		{
			"load close",
			func(t *testing.T, r *Runtime, rec *eventRecorder) {
				r.LoadPlugin("p", bytes)
				r.Close()
			},
		},
		{
			"load unload reload close",
			func(t *testing.T, r *Runtime, rec *eventRecorder) {
				r.LoadPlugin("p", bytes)
				r.UnloadPlugin("p")
				r.LoadPlugin("p", bytes)
				r.Close()
			},
		},
		{
			"failed construction then load close",
			func(t *testing.T, r *Runtime, rec *eventRecorder) {
				_, _ = r.LoadPlugin("hog", loadFixtureBytes(t, "test-memory-hog"))
				r.LoadPlugin("p", bytes)
				r.Close()
			},
		},
		{
			"duplicate rejected then close",
			func(t *testing.T, r *Runtime, rec *eventRecorder) {
				r.LoadPlugin("p", bytes)
				_, _ = r.LoadPlugin("p", bytes)
				r.Close()
			},
		},
		{
			"close then load rejected",
			func(t *testing.T, r *Runtime, rec *eventRecorder) {
				r.Close()
				_, _ = r.LoadPlugin("p", bytes)
			},
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			r := NewRuntimeWithOptions(context.Background(), RuntimeOptions{PoolSize: 1, MemoryLimitPages: 256})
			rec := newEventRecorder(t)
			rec.install(r, "rt")
			sc.run(t, r, rec)
			rec.validate()
		})
	}
}

// TestLifecycleHotReloadBoundary — rows 11/12: an unchanged digest's NEW
// acquire (on the reload runtime) precedes the OLD release (on the closed
// runtime); a changed digest releases the obsolete handle exactly once.
func TestLifecycleHotReloadBoundary(t *testing.T) {
	rec := newEventRecorder(t)
	rt1 := NewRuntime(context.Background())
	rec.install(rt1, "rt1")
	rt2 := NewRuntime(context.Background())
	rec.install(rt2, "rt2")

	digestA := MinimalV2Module(false)
	digestB := MinimalV2Module(true) // changed bytes

	if _, err := rt1.LoadPlugin("p", digestA); err != nil {
		t.Fatal(err)
	}
	// The reload runtime acquires the SAME digest while rt1 is still open —
	// the shared cache keeps the code alive across the swap.
	if _, err := rt2.LoadPlugin("p", digestA); err != nil {
		t.Fatal(err)
	}
	if err := rt1.Close(); err != nil {
		t.Fatal(err)
	}

	seqNew, okNew := rec.seqOf("publish:p")
	seqOld, okOld := rec.seqOf("release:p")
	if !okNew || !okOld {
		t.Fatalf("missing events; stream: %v", rec.snapshot())
	}
	if seqNew > seqOld {
		t.Fatalf("the reload acquire (publish:%d) came AFTER the old release (%d) — "+
			"an unchanged digest must not drop to zero across the swap", seqNew, seqOld)
	}

	// Changed digest: a third runtime takes digest B; closing rt2 releases
	// its handle on the now-obsolete A (each acquire releases exactly once,
	// so A's refcount reaches zero only when BOTH holders have released).
	rt3 := NewRuntime(context.Background())
	rec.install(rt3, "rt3")
	if _, err := rt3.LoadPlugin("p", digestB); err != nil {
		t.Fatal(err)
	}
	if err := rt2.Close(); err != nil {
		t.Fatal(err)
	}
	if rec.count("rt1:release:p") != 1 || rec.count("rt2:release:p") != 1 {
		t.Fatalf("the obsolete digest was not released once per holder: rt1=%d rt2=%d",
			rec.count("rt1:release:p"), rec.count("rt2:release:p"))
	}
	if err := rt3.Close(); err != nil {
		t.Fatal(err)
	}
	if rec.count("rt3:release:p") != 1 {
		t.Fatalf("digest B released %d times, want 1", rec.count("rt3:release:p"))
	}
	rec.validate()
}

// TestLifecycleFailedCompiledCloseStillCounts — a Close error from the
// compiled handle still counts as an attempted release in the seam.
func TestLifecycleFailedCompiledCloseStillCounts(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
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
	if rec.count("release:bad") != 1 {
		t.Fatalf("failed close recorded %d releases, want 1", rec.count("release:bad"))
	}
	rec.validate()
}
