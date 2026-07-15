package wasm

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

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
