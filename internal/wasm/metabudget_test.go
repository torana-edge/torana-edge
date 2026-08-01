package wasm

import (
	"context"
	"strings"
	"testing"
)

// Request metadata lives in the HOST, outside the guest's 64 MiB WASM cap, so
// an approved but buggy plugin could otherwise grow host memory until the
// request ended. These pin the budgets, including the case that was wrong.

func TestMetaSetNewKeyWithinBudget(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()
	if err := r.metaSetBounded(1, "k", strings.Repeat("x", 1024)); err != nil {
		t.Fatalf("a 1 KiB value was refused: %v", err)
	}
}

// The bug: the per-key check computed existing+delta, so REPLACING a value
// checked only the growth. Replacing 3.5 MiB with 7 MiB looked like a 3.5 MiB
// increase and was accepted, despite a 4 MiB per-key limit.
func TestMetaSetReplacementIsCheckedOnFinalSizeNotGrowth(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()

	small := strings.Repeat("x", 3_500_000) // 3.5 MiB, under the limit
	if err := r.metaSetBounded(1, "k", small); err != nil {
		t.Fatalf("3.5 MiB was refused: %v", err)
	}
	big := strings.Repeat("x", 7_000_000) // 7 MiB, over the 4 MiB per-key limit
	if err := r.metaSetBounded(1, "k", big); err == nil {
		t.Fatal("replacing 3.5 MiB with 7 MiB was accepted; the per-key limit was " +
			"applied to the growth rather than the final value")
	}
}

func TestMetaSetReplacementShrinkIsAllowed(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()
	if err := r.metaSetBounded(1, "k", strings.Repeat("x", 3_000_000)); err != nil {
		t.Fatal(err)
	}
	if err := r.metaSetBounded(1, "k", "small"); err != nil {
		t.Fatalf("shrinking a value was refused: %v", err)
	}
	got, present := r.metaGetPresence(1, "k")
	if !present || got != "small" {
		t.Fatalf("got %q present=%v, want the shrunk value", got, present)
	}
}

func TestMetaSetPerKeyOverflowIsRefused(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()
	if err := r.metaSetBounded(1, "k", strings.Repeat("x", maxMetaValueBytes+1)); err == nil {
		t.Fatal("a value over the per-key limit was accepted")
	}
	if _, present := r.metaGetPresence(1, "k"); present {
		t.Fatal("a refused write still stored the key")
	}
}

func TestMetaRequestTotalOverflowIsRefused(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()

	// Fill with keys each under the per-key limit until the request total is
	// exceeded — the limit no single key could trip.
	chunk := strings.Repeat("x", maxMetaValueBytes)
	var refused bool
	for i := 0; i < (maxMetaRequestBytes/maxMetaValueBytes)+2; i++ {
		if err := r.metaSetBounded(1, string(rune('a'+i)), chunk); err != nil {
			refused = true
			break
		}
	}
	if !refused {
		t.Fatal("the per-request total limit was never enforced")
	}
}

func TestMetaAppendRespectsThePerKeyLimit(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()

	chunk := make([]byte, 1<<20) // 1 MiB per fragment
	var err error
	for i := 0; i < 6; i++ { // 6 MiB total, over the 4 MiB per-key limit
		if _, _, err = r.metaAppend(1, "buf", chunk); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("appending past the per-key limit was accepted; a plugin could grow " +
			"host memory outside the guest's WASM cap")
	}
}

// The append must amortise. Building an exact-size slice per fragment, or
// concatenating strings, copies the whole buffer every time — O(total x
// fragments) on exactly the hot path this exists to serve.
//
// Measured by allocation count rather than wall time: a copy-per-fragment
// implementation allocates once per call and grows linearly, while append
// reuses spare capacity and grows logarithmically.
func TestMetaAppendAmortisesAcrossManySmallFragments(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()

	const fragments = 4096
	frag := []byte("0123456789abcdef") // 16 B, the shape a stream really sends

	allocs := testing.AllocsPerRun(1, func() {
		r.EndRequest(9)
		for i := 0; i < fragments; i++ {
			if _, _, err := r.metaAppend(9, "buf", frag); err != nil {
				t.Fatal(err)
			}
		}
	})

	// A copy-per-fragment implementation allocates at least once per fragment.
	// Amortised growth is far below that; the bound is deliberately loose so
	// this fails on a regression rather than on allocator noise.
	if allocs >= fragments {
		t.Fatalf("meta_append allocated %.0f times for %d fragments — it is copying "+
			"the whole buffer per call rather than growing in place", allocs, fragments)
	}

	got, present := r.metaGetPresence(9, "buf")
	if !present || len(got) != fragments*len(frag) {
		t.Fatalf("assembled buffer is %d bytes, want %d", len(got), fragments*len(frag))
	}
}
