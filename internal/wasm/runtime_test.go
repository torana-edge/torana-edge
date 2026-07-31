package wasm

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// timeoutPluginWasm is a minimal v2 guest whose run_hook loops forever.
//
// Built programmatically rather than as a hex blob: the previous literal had
// to be re-hand-assembled whenever an export or signature changed, and section
// lengths silently disagreeing with their contents produces a module that
// fails to instantiate for reasons unrelated to the test.
//
// It is intentionally tiny so cancellation coverage never depends on a
// compiler being installed in CI.
var timeoutPluginWasm = buildTimeoutPlugin()

func buildTimeoutPlugin() []byte {
	sec := func(id byte, body []byte) []byte {
		return append([]byte{id, byte(len(body))}, body...)
	}
	name := func(s string) []byte {
		return append([]byte{byte(len(s))}, s...)
	}

	// Types: 0 = (i32)->i32 for alloc, 1 = (i32,i32)->i64 for run_hook,
	// 2 = ()->i64 for supported_hooks. run_hook takes TWO arguments: v2 moved
	// the request id into HookInput.
	types := sec(0x01, []byte{
		0x03,
		0x60, 0x01, 0x7f, 0x01, 0x7f,
		0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7e,
		0x60, 0x00, 0x01, 0x7e,
	})
	funcs := sec(0x03, []byte{0x03, 0x00, 0x01, 0x02})
	mem := sec(0x05, []byte{0x01, 0x00, 0x01})

	exports := []byte{0x04}
	exports = append(exports, append(name("memory"), 0x02, 0x00)...)
	exports = append(exports, append(name("alloc"), 0x00, 0x00)...)
	exports = append(exports, append(name("run_hook"), 0x00, 0x01)...)
	exports = append(exports, append(name("supported_hooks"), 0x00, 0x02)...)

	code := sec(0x0a, []byte{
		0x03,
		0x04, 0x00, 0x41, 0x00, 0x0b, // alloc: i32.const 0
		0x08, 0x00, 0x03, 0x40, 0x0c, 0x00, 0x0b, 0x00, 0x0b, // run_hook: loop forever
		// supported_hooks: claim before-request so dispatch does not skip it.
		0x04, 0x00, 0x42, byte(pb.Hook_HOOK_BEFORE_REQUEST.Bit()), 0x0b,
	})

	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	out = append(out, types...)
	out = append(out, funcs...)
	out = append(out, mem...)
	out = append(out, sec(0x07, exports)...)
	return append(out, code...)
}

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
// from "I stored nothing" — the ambiguity v2 removes, and the reason absence is
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
