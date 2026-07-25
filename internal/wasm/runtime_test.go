package wasm

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// timeoutPluginWasm exports the Torana alloc ABI plus a hook that loops
// forever. It is intentionally tiny so cancellation coverage never depends on
// a compiler being installed in CI.
var timeoutPluginWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x0d, 0x02, 0x60, 0x01, 0x7f, 0x01, 0x7f,
	0x60, 0x03, 0x7e, 0x7e, 0x7e, 0x01, 0x7e,
	0x03, 0x03, 0x02, 0x00, 0x01,
	0x05, 0x03, 0x01, 0x00, 0x01,
	0x07, 0x19, 0x03,
	0x06, 0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, 0x02, 0x00,
	0x05, 0x61, 0x6c, 0x6c, 0x6f, 0x63, 0x00, 0x00,
	0x04, 0x6c, 0x6f, 0x6f, 0x70, 0x00, 0x01,
	0x0a, 0x0f, 0x02,
	0x04, 0x00, 0x41, 0x00, 0x0b,
	0x08, 0x00, 0x03, 0x40, 0x0c, 0x00, 0x0b, 0x00, 0x0b,
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
	err = p.CallRequest(context.Background(), "loop", 1, []byte("x"), &output)
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
func TestMetaEmptyValueDeletes(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()

	r.metaSet(1, "frag:x", "data")
	r.metaSet(1, "frag:x", "")
	if got := r.metaGet(1, "frag:x"); got != "" {
		t.Fatalf("got %q want deleted", got)
	}
	r.metaMu.RLock()
	_, exists := r.meta[1]["frag:x"]
	r.metaMu.RUnlock()
	if exists {
		t.Fatal("key still present after empty-value delete")
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
