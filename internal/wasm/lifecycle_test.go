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
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
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
	if ch := rec.barriers[ev]; ch != nil {
		close(ch)
	}
	rec.mu.Unlock()
}

// install wires the recorder into a runtime's observer seam. label
// distinguishes runtimes in multi-runtime tests (the reference model keys
// plugin state by runtime+name+generation).
func (rec *eventRecorder) install(r *Runtime, label string) {
	r.testHooks = &lifecycleHooks{
		loadBegin:        func(n string) { rec.record(label + ":load-begin:" + n) },
		compiledAcquired: func(n string) { rec.record(label + ":compiled-acquired:" + n) },
		published:        func(n string) { rec.record(label + ":published:" + n) },
		constructFailed:  func(n string) { rec.record(label + ":construct-failed:" + n) },
		instanceAcquired: func(n string) { rec.record(label + ":instance-acquired:" + n) },
		callFinished:     func(n string) { rec.record(label + ":call-finished:" + n) },
		compiledReleased: func(n string) { rec.record(label + ":compiled-released:" + n) },
		unloadBegin:      func(n string) { rec.record(label + ":unload-begin:" + n) },
		quiesced:         func(n string) { rec.record(label + ":quiesced:" + n) },
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

func (rec *eventRecorder) validate() {
	validateModel(rec.t, rec.snapshot())
}

// genState is one (runtime, name, generation) entry of the reference model.
// Phase and compiled-handle ownership are ORTHOGONAL: a construction failure
// can own a compiled handle before it ever publishes.
type genState struct {
	phase         string // absent | constructing | published | releasing | released
	compiledOwned bool
	activeCalls   int
}

// lifecycleModel is the small independent reference model. Runtime states
// open -> closing -> closed (per label). GENERATION HISTORY IS RETAINED:
// states are keyed by (label, name, generation) and terminal checks inspect
// every generation of a runtime, not only the current one.
type lifecycleModel struct {
	runtime map[string]string
	gens    map[string]*genState // key: label/name/gen
	gensOf  map[string]int       // key: label/name -> current generation
}

func newLifecycleModel() *lifecycleModel {
	return &lifecycleModel{runtime: map[string]string{}, gens: map[string]*genState{}, gensOf: map[string]int{}}
}

func (m *lifecycleModel) key(label, name string, gen int) string {
	return fmt.Sprintf("%s/%s/%d", label, name, gen)
}

func (m *lifecycleModel) observe(ev string) error {
	label := "rt"
	rest := ev
	if i := strings.Index(ev, ":"); i >= 0 {
		label = ev[:i]
		rest = ev[i+1:]
	}
	kind := rest
	name := ""
	if j := strings.Index(rest, ":"); j >= 0 {
		kind = rest[:j]
		name = rest[j+1:]
	}
	rtState := m.runtime[label]
	if rtState == "" {
		rtState = "open"
	}
	gen := m.gensOf[label+"/"+name]
	gs := m.gens[m.key(label, name, gen)]

	// Events that do not need a live generation.
	switch kind {
	case "load-begin":
		if rtState != "open" {
			return fmt.Errorf("load-begin while runtime %s", rtState)
		}
		if gs != nil && gs.phase != "absent" && gs.phase != "released" {
			return fmt.Errorf("load-begin of %s/%s with a live generation in %q (generation %d must end first)", label, name, gs.phase, gen)
		}
		gen++
		m.gensOf[label+"/"+name] = gen
		m.gens[m.key(label, name, gen)] = &genState{phase: "constructing"}
		return nil
	case "close-begin":
		if rtState != "open" {
			return fmt.Errorf("close-begin while runtime %s", rtState)
		}
		m.runtime[label] = "closing"
		return nil
	case "close-end":
		if rtState != "closing" {
			return fmt.Errorf("close-end while runtime %s", rtState)
		}
		// Terminal invariant over ALL retained generations of this runtime.
		for k, g := range m.gens {
			if !strings.HasPrefix(k, label+"/") {
				continue
			}
			if g.phase != "absent" && g.phase != "released" {
				return fmt.Errorf("close-end with %s in %q", k, g.phase)
			}
			if g.compiledOwned {
				return fmt.Errorf("close-end with %s still owning a compiled handle", k)
			}
			if g.activeCalls != 0 {
				return fmt.Errorf("close-end with %d active calls on %s", g.activeCalls, k)
			}
		}
		m.runtime[label] = "closed"
		return nil
	}
	if gs == nil {
		if kind == "quiesced" {
			// An INJECTED plugin (tests construct one directly in the map) is
			// published-and-owned by construction; its quiescence is legal and
			// enters releasing, so the subsequent compiled-released must still
			// cross the quiesced boundary.
			gen++
			m.gensOf[label+"/"+name] = gen
			m.gens[m.key(label, name, gen)] = &genState{phase: "releasing", compiledOwned: true}
			return nil
		}
		return fmt.Errorf("%s for unknown generation of %s/%s", kind, label, name)
	}

	switch kind {
	case "compiled-acquired":
		if gs.phase != "constructing" {
			return fmt.Errorf("compiled-acquired of %s/%s gen %d from %q", label, name, gen, gs.phase)
		}
		if gs.compiledOwned {
			return fmt.Errorf("duplicate compiled-acquired of %s/%s", label, name)
		}
		gs.compiledOwned = true
	case "published":
		if gs.phase != "constructing" {
			return fmt.Errorf("published of %s/%s gen %d from %q", label, name, gen, gs.phase)
		}
		if !gs.compiledOwned {
			return fmt.Errorf("published of %s/%s without compiled ownership", label, name)
		}
		gs.phase = "published"
	case "construct-failed":
		if gs.phase != "constructing" {
			return fmt.Errorf("construct-failed of %s/%s gen %d from %q", label, name, gen, gs.phase)
		}
		if gs.compiledOwned {
			return fmt.Errorf("construct-failed of %s/%s while still owning the compiled handle", label, name)
		}
		if gs.activeCalls != 0 {
			return fmt.Errorf("construct-failed of %s/%s with active calls", label, name)
		}
		gs.phase = "absent"
	case "instance-acquired":
		// Cleanup intent may overlap calls that win admission BEFORE that
		// plugin's write lock is acquired: the per-generation QUIESCED event
		// is the boundary, not close-begin. Production global no-new-request
		// admission is PluginPipeline.DrainAndClose's job.
		if gs.phase != "published" {
			return fmt.Errorf("instance-acquired of %s/%s gen %d from %q", label, name, gen, gs.phase)
		}
		gs.activeCalls++
	case "call-finished":
		// Legal only while published: after quiescence no active call can
		// exist to finish (the write lock is held from quiesced onward).
		if gs.phase != "published" {
			return fmt.Errorf("call-finished of %s/%s gen %d from %q", label, name, gen, gs.phase)
		}
		if gs.activeCalls <= 0 {
			return fmt.Errorf("call-finished of %s/%s without an active call", label, name)
		}
		gs.activeCalls--
	case "unload-begin":
		// Intent only: this fires BEFORE the plugin's write lock, so calls
		// may legally win admission in that interval (exactly like the
		// accepted close interleaving). Validate the generation is published
		// and LEAVE it published: quiesced performs published -> releasing.
		if gs.phase != "published" {
			return fmt.Errorf("unload-begin of %s/%s gen %d from %q", label, name, gen, gs.phase)
		}
		// no state change
	case "quiesced":
		// The per-plugin quiescence boundary: the write lock is held, so no
		// active calls remain and no further acquisition is legal.
		if gs.phase != "published" && gs.phase != "releasing" {
			return fmt.Errorf("quiesced of %s/%s gen %d from %q", label, name, gen, gs.phase)
		}
		if gs.activeCalls != 0 {
			return fmt.Errorf("quiesced of %s/%s with %d active calls", label, name, gs.activeCalls)
		}
		gs.phase = "releasing"
	case "compiled-released":
		// quiesced is the mandatory normal-release boundary: construction
		// cleanup releases from constructing, and every other release must
		// come from releasing (entered ONLY by quiesced). A published
		// generation can never release without quiescence.
		if !gs.compiledOwned {
			return fmt.Errorf("duplicate compiled-released of %s/%s", label, name)
		}
		if gs.phase != "constructing" && gs.phase != "releasing" {
			return fmt.Errorf("compiled-released of %s/%s gen %d from %q (release requires quiesced)", label, name, gen, gs.phase)
		}
		if gs.activeCalls != 0 {
			return fmt.Errorf("compiled-released of %s/%s with %d active calls", label, name, gs.activeCalls)
		}
		gs.compiledOwned = false
		if gs.phase != "constructing" {
			gs.phase = "released" // close/unload completion
		}
		// constructing: construct-failed must follow to end absent.
	default:
		return fmt.Errorf("unknown event %q", ev)
	}
	return nil
}

// validateModel runs the model over a raw event stream (label:kind:name
// suffixes; the leading sequence number is stripped) and asserts the runtime
// ends CLOSED — a scenario that promised a terminal close must actually
// complete one.
func validateModel(t *testing.T, events []string) {
	t.Helper()
	m := newLifecycleModel()
	labels := map[string]bool{}
	for _, ev := range events {
		ev = ev[strings.Index(ev, ":")+1:]
		labels[ev[:strings.Index(ev, ":")]] = true
		if err := m.observe(ev); err != nil {
			t.Fatalf("reference model rejected %q: %v\nstream: %v", ev, err, events)
		}
	}
	for label := range labels {
		if m.runtime[label] != "closed" {
			t.Fatalf("runtime %s ended in %q, want closed (an unfinished stream cannot be a terminal proof)", label, m.runtime[label])
		}
	}
}

// TestLifecycleCloseReleasesCompiledExactlyOnce — every acquired compiled
// handle is released exactly once; repeated Close never increments release
// counts; the recorded stream satisfies the reference model.
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
	if rec.count("rt:compiled-released:inert-a") != 1 || rec.count("rt:compiled-released:observer") != 1 {
		t.Fatalf("release counts = inert-a:%d observer:%d, want 1 each",
			rec.count("rt:compiled-released:inert-a"), rec.count("rt:compiled-released:observer"))
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if rec.count("rt:compiled-released:inert-a") != 1 || rec.count("rt:compiled-released:observer") != 1 {
		t.Fatal("repeated Close incremented release counts")
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
	if rec.count("rt:compiled-released:inert-a") != 1 {
		t.Fatalf("concurrent Close released %d times, want 1", rec.count("rt:compiled-released:inert-a"))
	}
	rec.validate()
}

// TestLifecycleLoadAfterCloseFails — LoadPlugin and UnloadPlugin after Close
// fail/no-op deterministically and never acquire resources.
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
	if rec.fired("rt:load-begin:observer") {
		t.Fatal("post-close load began a construction transaction")
	}
	rec.validate()
}

// TestLifecycleLoadVsCloseOrderings — both LEGAL orderings are proven with
// deterministic barriers instead of a scheduler race.
func TestLifecycleLoadVsCloseOrderings(t *testing.T) {
	t.Run("close first", func(t *testing.T) {
		r := newTestRuntime(t)
		rec := newEventRecorder(t)
		rec.install(r, "rt")
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 4; i++ {
			if _, err := r.LoadPlugin(fmt.Sprintf("after-%d", i), MinimalModule(false)); err == nil {
				t.Fatal("load succeeded after close")
			}
		}
		if rec.fired("rt:load-begin:after-0") {
			t.Fatal("a load after close began a construction transaction")
		}
		rec.validate()
	})
	t.Run("load first", func(t *testing.T) {
		r := newTestRuntime(t)
		rec := newEventRecorder(t)
		rec.install(r, "rt")
		for i := 0; i < 3; i++ {
			if _, err := r.LoadPlugin(fmt.Sprintf("before-%d", i), MinimalModule(false)); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if rec.count(fmt.Sprintf("rt:compiled-released:before-%d", i)) != 1 {
				t.Fatalf("before-%d released %d times, want 1", i, rec.count(fmt.Sprintf("rt:compiled-released:before-%d", i)))
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
	if rec.count("rt:compiled-released:inert-a") != 1 {
		t.Fatalf("duplicate load released %d times, want 1", rec.count("rt:compiled-released:inert-a"))
	}
	rec.validate()
}

// TestLifecycleConcurrentDuplicateLoads — at most one of N concurrent loads
// publishes; the losers fail as duplicates without compiling.
func TestLifecycleConcurrentDuplicateLoads(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
	bytes := MinimalModule(false)
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
	if rec.count("rt:compiled-released:dup") != 1 {
		t.Fatalf("concurrent duplicates released %d times, want 1", rec.count("rt:compiled-released:dup"))
	}
	rec.validate()
}

// TestLifecycleConstructionFailureReleasesCompiled — a hand-built module that
// traps at INIT compiles successfully and fails during instantiation: the
// stream is load-begin -> compiled-acquired -> compiled-released ->
// construct-failed, proving the post-compile path releases the handle.
func TestLifecycleConstructionFailureReleasesCompiled(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
	_, err := r.LoadPlugin("trap", MinimalModuleTrapsAtInit())
	if err == nil {
		t.Fatal("a module that traps at init loaded")
	}
	if strings.Contains(err.Error(), "compile") {
		t.Fatalf("the trap module failed at COMPILE (err: %v) — the test must exercise the instantiation path", err)
	}
	for _, ev := range []string{"rt:compiled-acquired:trap", "rt:compiled-released:trap", "rt:construct-failed:trap"} {
		if !rec.fired(ev) {
			t.Fatalf("missing event %s; stream: %v", ev, rec.snapshot())
		}
	}
	seqAcq, _ := rec.seqOf("rt:compiled-acquired:trap")
	seqRel, _ := rec.seqOf("rt:compiled-released:trap")
	seqFail, _ := rec.seqOf("rt:construct-failed:trap")
	if !(seqAcq < seqRel && seqRel < seqFail) {
		t.Fatalf("construction-failure order wrong: acquired=%d released=%d failed=%d", seqAcq, seqRel, seqFail)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	rec.validate()
}

// TestLifecycleHookDiscoveryFailureReleasesCompiled — a guest that compiles
// and instantiates but exports no supported_hooks fails hook discovery; the
// error path releases BOTH the instance and the compiled handle and joins the
// cleanup errors with the primary error.
func TestLifecycleHookDiscoveryFailureReleasesCompiled(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
	_, err := r.LoadPlugin("nohooks", MinimalModuleNoHooks())
	if err == nil {
		t.Fatal("a guest without supported_hooks loaded")
	}
	if !strings.Contains(err.Error(), "supported_hooks") {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.count("rt:compiled-released:nohooks") != 1 {
		t.Fatalf("hook-discovery failure released compiled %d times, want 1", rec.count("rt:compiled-released:nohooks"))
	}
	if !rec.fired("rt:construct-failed:nohooks") {
		t.Fatal("hook-discovery failure did not end construction with construct-failed")
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

// TestLifecycleCleanupErrorsJoinedAndSorted — one failed close does not block
// the others; the failing plugin's error surfaces and the good plugin's
// release is still attempted.
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
	if rec.count("rt:compiled-released:a-good") != 1 {
		t.Fatalf("a-good was not released despite b-failing's error")
	}
	if rec.count("rt:compiled-released:b-failing") != 1 {
		t.Fatalf("b-failing was not released")
	}
	rec.validate()
}

// TestLifecycleInstancesClosedBeforeCompiled — instances are closed before
// the compiled handle.
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

// TestLifecycleActiveCallBlocksCompiledRelease — quiescence: a call that has
// ACQUIRED an instance (the observer barrier blocks it post-acquire, while
// callMu.RLock is held) must be quiesced by the admission lock — Close's
// write lock cannot release the compiled handle until the call finishes, and
// the event order is instance-acquired -> close-begin -> call-finished ->
// compiled-released, with exactly one release.
func TestLifecycleActiveCallBlocksCompiledRelease(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
	acquired := make(chan struct{})
	release := make(chan struct{})
	// Override the observer: signal + block INSIDE CallRequest while the
	// admission read lock is held (no recorder mutex is held while blocked).
	r.testHooks.instanceAcquired = func(name string) {
		rec.record("rt:instance-acquired:" + name)
		close(acquired)
		<-release
	}

	p, err := r.LoadPlugin("fast", MinimalModule(false))
	if err != nil {
		t.Fatal(err)
	}
	out := []byte{}
	done := make(chan error, 1)
	go func() {
		done <- p.CallRequest(context.Background(), pb.Hook_HOOK_BEFORE_REQUEST, 1, []byte{}, &out)
	}()
	<-acquired // deterministic: the call holds an instance under callMu.RLock

	closed := make(chan struct{})
	go func() {
		if err := r.Close(); err != nil {
			t.Errorf("Close during an active call: %v", err)
		}
		close(closed)
	}()
	rec.wait("rt:close-begin")

	// State checks after the deterministic barrier — no timing assertions.
	select {
	case <-closed:
		t.Fatal("Close completed while the call was still blocked")
	default:
	}
	if rec.fired("rt:compiled-released:fast") {
		t.Fatal("compiled handle released while a call was active")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the call after the barrier: %v", err)
	}
	<-closed

	if rec.count("rt:compiled-released:fast") != 1 {
		t.Fatalf("compiled released %d times, want exactly 1", rec.count("rt:compiled-released:fast"))
	}
	seqInst, _ := rec.seqOf("rt:instance-acquired:fast")
	seqClose, _ := rec.seqOf("rt:close-begin")
	seqFinish, _ := rec.seqOf("rt:call-finished:fast")
	seqQuiesced, _ := rec.seqOf("rt:quiesced:fast")
	seqRelease, _ := rec.seqOf("rt:compiled-released:fast")
	if !(seqInst < seqClose && seqClose < seqFinish && seqFinish < seqQuiesced && seqQuiesced < seqRelease) {
		t.Fatalf("event order wrong: instance=%d close-begin=%d call-finish=%d quiesced=%d release=%d",
			seqInst, seqClose, seqFinish, seqQuiesced, seqRelease)
	}
	rec.validate()
}

// TestLifecycleUnloadBlocksUntilCallsQuiesce — UnloadPlugin takes the same
// admission write lock; an active call delays the unload until it finishes,
// and reachability stays intact while the call's host calls could resolve it.
func TestLifecycleUnloadBlocksUntilCallsQuiesce(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
	acquired := make(chan struct{})
	release := make(chan struct{})
	r.testHooks.instanceAcquired = func(name string) {
		rec.record("rt:instance-acquired:" + name)
		close(acquired)
		<-release
	}

	p, err := r.LoadPlugin("fast", MinimalModule(false))
	if err != nil {
		t.Fatal(err)
	}
	out := []byte{}
	done := make(chan error, 1)
	go func() {
		done <- p.CallRequest(context.Background(), pb.Hook_HOOK_BEFORE_REQUEST, 1, []byte{}, &out)
	}()
	<-acquired

	unloaded := make(chan error, 1)
	go func() {
		unloaded <- r.UnloadPlugin("fast")
	}()
	rec.wait("rt:unload-begin:fast")
	select {
	case <-unloaded:
		t.Fatal("unload completed while a call was still blocked")
	default:
	}
	if rec.fired("rt:compiled-released:fast") {
		t.Fatal("unload released the compiled handle while a call was active")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the call after the barrier: %v", err)
	}
	if err := <-unloaded; err != nil {
		t.Fatal(err)
	}
	if rec.count("rt:compiled-released:fast") != 1 {
		t.Fatalf("unload released compiled %d times, want 1", rec.count("rt:compiled-released:fast"))
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	rec.validate()
}

// TestLifecycleNoPublishedPluginsAfterClose — the map is empty once close
// completes (each plugin removed after its own quiescence).
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
// reachability after quiescence, releases exactly once, and a second unload
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
	if rec.count("rt:compiled-released:inert-a") != 1 {
		t.Fatalf("unload released compiled %d times, want exactly 1", rec.count("rt:compiled-released:inert-a"))
	}
	if _, err := r.LoadPlugin("inert-a", loadFixtureBytes(t, "test-inert-a")); err != nil {
		t.Fatalf("reload after unload failed: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if rec.count("rt:compiled-released:inert-a") != 2 {
		t.Fatalf("two loads/unloads released %d times, want 2", rec.count("rt:compiled-released:inert-a"))
	}
	rec.validate()
}

// TestLifecycleReferenceModelScenarios — the reference model, driven through
// the REAL runtime for a table of scenarios. Every operation's result is
// ASSERTED (the intentionally failing calls assert their failure class, all
// others require success), and validate() additionally requires the runtime
// to end closed — a truncated scenario can never pass as a prefix.
func TestLifecycleReferenceModelScenarios(t *testing.T) {
	bytes := MinimalModule(false)
	scenarios := []struct {
		name string
		run  func(t *testing.T, r *Runtime) error
		// check runs after the scenario, before the model validates the
		// recorded stream.
		check func(t *testing.T, rec *eventRecorder)
	}{
		{
			"load close",
			func(t *testing.T, r *Runtime) error {
				if _, err := r.LoadPlugin("p", bytes); err != nil {
					return fmt.Errorf("load: %w", err)
				}
				return r.Close()
			},
			nil,
		},
		{
			"load unload reload close",
			func(t *testing.T, r *Runtime) error {
				if _, err := r.LoadPlugin("p", bytes); err != nil {
					return fmt.Errorf("load: %w", err)
				}
				if err := r.UnloadPlugin("p"); err != nil {
					return fmt.Errorf("unload: %w", err)
				}
				if _, err := r.LoadPlugin("p", bytes); err != nil {
					return fmt.Errorf("reload: %w", err)
				}
				return r.Close()
			},
			nil,
		},
		{
			"compile failure then load close",
			func(t *testing.T, r *Runtime) error {
				// Deliberately invalid WASM: magic bytes only, no version or
				// sections. wazero rejects it at CompileModule, so the load
				// must fail specifically at compile.
				_, err := r.LoadPlugin("bad", []byte{0x00, 0x61, 0x73, 0x6d})
				if err == nil || !strings.Contains(err.Error(), "compile") {
					return fmt.Errorf("invalid WASM must fail at compile, got %v", err)
				}
				if _, err := r.LoadPlugin("bad", bytes); err != nil {
					return fmt.Errorf("load after compile failure: %w", err)
				}
				return r.Close()
			},
			func(t *testing.T, rec *eventRecorder) {
				// The first generation's transaction ends at construct-failed
				// with NO acquire or release in its window (the later
				// acquire/release belongs to the successful reload of the
				// same name).
				_, failOk := rec.seqOf("rt:construct-failed:bad")
				loadSeq, loadOk := rec.seqOf("rt:load-begin:bad")
				if !failOk || !loadOk {
					t.Fatalf("expected load-begin and construct-failed for the failed load")
				}
				for _, ev := range rec.snapshot() {
					var seq int
					fmt.Sscanf(ev, "%d:", &seq)
					if seq < loadSeq {
						continue
					}
					if strings.HasSuffix(ev, "rt:compiled-acquired:bad") || strings.HasSuffix(ev, "rt:compiled-released:bad") {
						t.Fatalf("the failed-load window acquired or released a compiled handle: %s", ev)
					}
					if strings.HasSuffix(ev, "rt:construct-failed:bad") {
						break // window ends at construct-failed
					}
				}
			},
		},
		{
			"failed construction then load close",
			func(t *testing.T, r *Runtime) error {
				_, err := r.LoadPlugin("trap", MinimalModuleTrapsAtInit())
				if err == nil || strings.Contains(err.Error(), "compile") {
					return fmt.Errorf("trap load must fail at instantiation, got %v", err)
				}
				if _, err := r.LoadPlugin("p", bytes); err != nil {
					return fmt.Errorf("load after failed construction: %w", err)
				}
				return r.Close()
			},
			nil,
		},
		{
			"duplicate rejected then close",
			func(t *testing.T, r *Runtime) error {
				if _, err := r.LoadPlugin("p", bytes); err != nil {
					return fmt.Errorf("load: %w", err)
				}
				_, err := r.LoadPlugin("p", bytes)
				if err == nil || !strings.Contains(err.Error(), "already loaded") {
					return fmt.Errorf("duplicate load must fail as already-loaded, got %v", err)
				}
				return r.Close()
			},
			nil,
		},
		{
			"close then load rejected",
			func(t *testing.T, r *Runtime) error {
				if err := r.Close(); err != nil {
					return err
				}
				_, err := r.LoadPlugin("p", bytes)
				if err == nil || !strings.Contains(err.Error(), "runtime is closed") {
					return fmt.Errorf("post-close load must fail as closed, got %v", err)
				}
				return nil
			},
			nil,
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			r := newTestRuntime(t)
			rec := newEventRecorder(t)
			rec.install(r, "rt")
			if err := sc.run(t, r); err != nil {
				t.Fatal(err)
			}
			if sc.check != nil {
				sc.check(t, rec)
			}
			rec.validate() // also asserts the runtime ended closed
		})
	}
}

// TestLifecycleHotReloadBoundary — an unchanged digest's NEW acquire (the
// reload runtime's published event) precedes the OLD release (the closed
// runtime's compiled-released); a changed digest releases the obsolete handle
// once per holder.
func TestLifecycleHotReloadBoundary(t *testing.T) {
	rec := newEventRecorder(t)
	rt1 := NewRuntime(context.Background())
	rec.install(rt1, "rt1")
	rt2 := NewRuntime(context.Background())
	rec.install(rt2, "rt2")

	digestA := MinimalModule(false)
	digestB := MinimalModule(true) // changed bytes

	if _, err := rt1.LoadPlugin("p", digestA); err != nil {
		t.Fatal(err)
	}
	// The reload runtime acquires the SAME digest while rt1 is still open.
	if _, err := rt2.LoadPlugin("p", digestA); err != nil {
		t.Fatal(err)
	}
	if err := rt1.Close(); err != nil {
		t.Fatal(err)
	}

	// Exact generation-labelled events — never suffix-first matches.
	seqNew, okNew := rec.seqOf("rt2:published:p")
	seqOld, okOld := rec.seqOf("rt1:compiled-released:p")
	if !okNew || !okOld {
		t.Fatalf("missing labelled events; stream: %v", rec.snapshot())
	}
	if seqNew > seqOld {
		t.Fatalf("the reload acquire (rt2:published:%d) came AFTER the old release (rt1:%d) — "+
			"an unchanged digest must not drop to zero across the swap", seqNew, seqOld)
	}

	// Changed digest: a third runtime takes digest B; closing rt2 releases its
	// handle on the now-obsolete A (one release per holder).
	rt3 := NewRuntime(context.Background())
	rec.install(rt3, "rt3")
	if _, err := rt3.LoadPlugin("p", digestB); err != nil {
		t.Fatal(err)
	}
	if err := rt2.Close(); err != nil {
		t.Fatal(err)
	}
	if rec.count("rt1:compiled-released:p") != 1 || rec.count("rt2:compiled-released:p") != 1 {
		t.Fatalf("the obsolete digest was not released once per holder: rt1=%d rt2=%d",
			rec.count("rt1:compiled-released:p"), rec.count("rt2:compiled-released:p"))
	}
	if err := rt3.Close(); err != nil {
		t.Fatal(err)
	}
	if rec.count("rt3:compiled-released:p") != 1 {
		t.Fatalf("digest B released %d times, want 1", rec.count("rt3:compiled-released:p"))
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
	if rec.count("rt:compiled-released:bad") != 1 {
		t.Fatalf("failed close recorded %d releases, want 1", rec.count("rt:compiled-released:bad"))
	}
	rec.validate()
}

// TestLifecycleCloseKeepsReachabilityUntilQuiescence — F1: the quiesced
// observer fires UNDER the transaction's write lock, immediately after the
// exact-pointer deletion and BEFORE any resource release. It is used as a
// deterministic in-transaction barrier: while it is held, the plugin is
// absent from the map, its resources are NOT yet closed, no release has
// fired, and a stale call cannot pass the held write lock. Releasing the
// barrier lets cleanup complete. This pins delete-before-close (and the
// absence of any published-but-closed interval) without sleeps.
func TestLifecycleCloseKeepsReachabilityUntilQuiescence(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
	acquired := make(chan struct{})
	releaseCall := make(chan struct{})
	quiesced := make(chan struct{})
	releaseQuiesce := make(chan struct{})
	r.testHooks.instanceAcquired = func(name string) {
		rec.record("rt:instance-acquired:" + name)
		close(acquired)
		<-releaseCall
	}
	r.testHooks.quiesced = func(name string) {
		rec.record("rt:quiesced:" + name)
		close(quiesced)
		<-releaseQuiesce
	}

	p, err := r.LoadPlugin("fast", MinimalModule(false))
	if err != nil {
		t.Fatal(err)
	}
	out := []byte{}
	done := make(chan error, 1)
	go func() {
		done <- p.CallRequest(context.Background(), pb.Hook_HOOK_BEFORE_REQUEST, 1, []byte{}, &out)
	}()
	<-acquired

	closed := make(chan struct{})
	go func() {
		if err := r.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		close(closed)
	}()
	rec.wait("rt:close-begin")

	// While Close waits for the write lock, the plugin is still published and
	// its resources are NOT closed (no published-but-closed interval).
	r.mu.RLock()
	_, stillPublished := r.plugins["fast"]
	r.mu.RUnlock()
	if !stillPublished {
		t.Fatal("plugin removed from the map BEFORE quiescence")
	}
	p.stateMu.RLock()
	poolClosed := p.poolClosed
	p.stateMu.RUnlock()
	if poolClosed {
		t.Fatal("pool closed before quiescence")
	}

	close(releaseCall)
	if err := <-done; err != nil {
		t.Fatalf("the active call: %v", err)
	}
	// Close acquires the write lock, deletes the exact pointer, and fires
	// quiesced: the in-transaction barrier now holds the write lock.
	<-quiesced

	r.mu.RLock()
	_, stillPublishedAtQuiesced := r.plugins["fast"]
	r.mu.RUnlock()
	if stillPublishedAtQuiesced {
		t.Fatal("plugin still published AT quiescence (deletion must precede it)")
	}
	p.stateMu.RLock()
	poolClosedAtQuiesced := p.poolClosed
	p.stateMu.RUnlock()
	if poolClosedAtQuiesced {
		t.Fatal("pool closed before the release phase of the transaction")
	}
	if rec.count("rt:compiled-released:fast") != 0 {
		t.Fatalf("compiled released before quiescence: %d", rec.count("rt:compiled-released:fast"))
	}

	// A stale call cannot pass the write lock the barrier holds: the lock is
	// deterministically observable, so assert it directly rather than racing
	// a goroutine against the scheduler.
	if p.callMu.TryRLock() {
		p.callMu.RUnlock()
		t.Fatal("a stale call could acquire the read lock while the write lock is held")
	}

	close(releaseQuiesce)
	if err := p.CallRequest(context.Background(), pb.Hook_HOOK_BEFORE_REQUEST, 1, []byte{}, &out); err == nil || !strings.Contains(err.Error(), "plugin is closed") {
		t.Fatalf("a stale-pointer call after Close returned %v, want a plugin-is-closed error", err)
	}
	<-closed

	r.mu.RLock()
	_, still := r.plugins["fast"]
	r.mu.RUnlock()
	if still {
		t.Fatal("plugin still published after Close")
	}
	if rec.count("rt:compiled-released:fast") != 1 {
		t.Fatalf("released %d times, want 1", rec.count("rt:compiled-released:fast"))
	}
	rec.validate()
}

// TestLifecycleCloseAllowsWinningAdmissionUntilQuiesced — F2: cleanup intent
// (close-begin) may overlap calls that win admission before THAT plugin's
// write lock is acquired. The model's boundary is the per-generation quiesced
// event, not the global close-begin.
func TestLifecycleCloseAllowsWinningAdmissionUntilQuiesced(t *testing.T) {
	r := newTestRuntime(t)
	rec := newEventRecorder(t)
	rec.install(r, "rt")
	acquiredA := make(chan struct{})
	releaseA := make(chan struct{})
	r.testHooks.instanceAcquired = func(name string) {
		rec.record("rt:instance-acquired:" + name)
		if name == "a" {
			close(acquiredA)
			<-releaseA
		}
	}

	for _, n := range []string{"a", "b"} {
		if _, err := r.LoadPlugin(n, MinimalModule(false)); err != nil {
			t.Fatal(err)
		}
	}
	outA := []byte{}
	doneA := make(chan error, 1)
	go func() {
		doneA <- r.plugins["a"].CallRequest(context.Background(), pb.Hook_HOOK_BEFORE_REQUEST, 1, []byte{}, &outA)
	}()
	<-acquiredA

	closed := make(chan struct{})
	go func() {
		if err := r.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		close(closed)
	}()
	rec.wait("rt:close-begin")

	// b wins admission AFTER close-begin but BEFORE its own write lock:
	// the runtime produces this legal trace, and the model must accept it.
	outB := []byte{}
	if err := r.plugins["b"].CallRequest(context.Background(), pb.Hook_HOOK_BEFORE_REQUEST, 1, []byte{}, &outB); err != nil {
		t.Fatalf("b could not win admission after close-begin: %v", err)
	}

	close(releaseA)
	if err := <-doneA; err != nil {
		t.Fatalf("call a: %v", err)
	}
	<-closed
	rec.validate() // the model accepts b's instance-acquired between close-begin and b's quiesced
}

// TestLifecycleModelNegativeMatrix — F3: the model's rejection branches are
// proven adversarial. Every row is an EXPLICIT event sequence whose FINAL
// event must be rejected (all prefix events must succeed) with a stable
// error substring naming the intended invariant; rows whose terminal-guard
// state is deliberately unreachable through legal transitions seed the model
// state directly (that is exactly why direct setup is appropriate there).
func TestLifecycleModelNegativeMatrix(t *testing.T) {
	pub := []string{"rt:load-begin:n", "rt:compiled-acquired:n", "rt:published:n"}
	rows := []struct {
		name    string
		events  []string
		wantErr string // stable substring of the rejection
		arrange func(m *lifecycleModel)
	}{
		{"publish without compiled ownership", []string{"rt:load-begin:n", "rt:published:n"}, "without compiled ownership", nil},
		{"duplicate compiled acquire", append(pub, "rt:compiled-acquired:n"), "compiled-acquired of rt/n gen 1 from \"published\"", nil},
		{"duplicate compiled release", append(append(pub, "rt:quiesced:n", "rt:compiled-released:n"), "rt:compiled-released:n"), "duplicate compiled-released", nil},
		{"published release without quiescence", append(pub, "rt:compiled-released:n"), "release requires quiesced", nil},
		{"call finish without acquisition", append(pub, "rt:call-finished:n"), "without an active call", nil},
		{"quiesce with active calls", append(pub, "rt:instance-acquired:n", "rt:quiesced:n"), "with 1 active calls", nil},
		{"release while active", []string{"rt:close-begin", "rt:compiled-released:n"}, "with 1 active calls", func(m *lifecycleModel) {
			m.gensOf["rt/n"] = 1
			m.gens[m.key("rt", "n", 1)] = &genState{phase: "releasing", compiledOwned: true, activeCalls: 1}
		}},
		{"construct failed while compiled-owned", []string{"rt:load-begin:n", "rt:compiled-acquired:n", "rt:construct-failed:n"}, "still owning the compiled handle", nil},
		{"load after close begins", append(pub, "rt:close-begin", "rt:load-begin:n"), "load-begin while runtime closing", nil},
		{"unload intent without published", append(pub, "rt:quiesced:n", "rt:unload-begin:n"), "unload-begin of rt/n gen 1 from \"releasing\"", nil},
		{"instance after quiesced", append(pub, "rt:quiesced:n", "rt:instance-acquired:n"), "instance-acquired of rt/n gen 1 from \"releasing\"", nil},
		{"close-end with live phase", append(pub, "rt:close-begin", "rt:close-end"), "close-end with rt/n/1 in \"published\"", nil},
		{"close-end with owned handle", []string{"rt:close-begin", "rt:close-end"}, "still owning a compiled handle", func(m *lifecycleModel) {
			m.gensOf["rt/n"] = 1
			m.gens[m.key("rt", "n", 1)] = &genState{phase: "released", compiledOwned: true}
		}},
		{"close-end with active calls", []string{"rt:close-begin", "rt:close-end"}, "active calls", func(m *lifecycleModel) {
			m.gensOf["rt/n"] = 1
			m.gens[m.key("rt", "n", 1)] = &genState{phase: "released", activeCalls: 1}
		}},
		{"generation N+1 before N ends", append(pub, "rt:load-begin:n"), "with a live generation in \"published\"", nil},
		{"call finish twice", append(pub, "rt:instance-acquired:n", "rt:call-finished:n", "rt:call-finished:n"), "without an active call", nil},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			m := newLifecycleModel()
			if row.arrange != nil {
				row.arrange(m)
			}
			for i, ev := range row.events {
				err := m.observe(ev)
				if i == len(row.events)-1 {
					if err == nil {
						t.Fatalf("the final event %q was accepted; it must be rejected", ev)
					}
					if row.wantErr != "" && !strings.Contains(err.Error(), row.wantErr) {
						t.Fatalf("rejection %q does not contain %q", err.Error(), row.wantErr)
					}
					return
				}
				if err != nil {
					t.Fatalf("prefix event %q errored early: %v", ev, err)
				}
			}
		})
	}
}

// TestLifecycleModelUnloadAdmissionWindowAccepted — positive trace: a call
// wins admission between unload-begin (intent, fired before the write lock)
// and quiesced (the boundary). The model must accept the full stream.
func TestLifecycleModelUnloadAdmissionWindowAccepted(t *testing.T) {
	m := newLifecycleModel()
	events := []string{
		"rt:load-begin:n",
		"rt:compiled-acquired:n",
		"rt:published:n",
		"rt:unload-begin:n", // intent only: generation stays published
		"rt:instance-acquired:n",
		"rt:call-finished:n",
		"rt:quiesced:n",
		"rt:compiled-released:n",
		"rt:close-begin",
		"rt:close-end",
	}
	for _, ev := range events {
		if err := m.observe(ev); err != nil {
			t.Fatalf("legal unload-interleaving stream rejected at %q: %v", ev, err)
		}
	}
}
