package pluginstate

import (
	"path/filepath"
	"strconv"
	"testing"
)

// The package doc quotes what a Set costs, and a quoted number that nobody can
// re-run is a number that ages into a lie. These are the benchmarks it was
// measured with.
//
// They also pin the shape of the claim rather than the numbers, which are
// machine-specific: a durable Set costs an order of magnitude more than the
// same write with no file behind it, and the cost tracks the size of the whole
// store rather than the value being written.

func seedStore(tb testing.TB, s *Store, n int) {
	tb.Helper()
	for i := 0; i < n; i++ {
		if err := s.Set("warmer", "prefix"+strconv.Itoa(i), "some stored prefix value"); err != nil {
			tb.Fatal(err)
		}
	}
}

// BenchmarkSetDurable is one Set against a store of 500 resident keys, with
// the full temp-file/fsync/rename/dir-fsync transaction.
func BenchmarkSetDurable(b *testing.B) {
	s, err := New(Options{Path: filepath.Join(b.TempDir(), "plugin-state.json")})
	if err != nil {
		b.Fatal(err)
	}
	seedStore(b, s, 500)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Set("ratelimiter", "counter", strconv.Itoa(i)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSetMemoryOnly is the same write with no file behind it, so the
// difference between the two is the disk transaction.
func BenchmarkSetMemoryOnly(b *testing.B) {
	s, err := New(Options{})
	if err != nil {
		b.Fatal(err)
	}
	seedStore(b, s, 500)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Set("ratelimiter", "counter", strconv.Itoa(i)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSetDurableSmallStore is the same durable write against a nearly
// empty store. Comparing it with BenchmarkSetDurable shows the cost tracking
// the store's size rather than the value written — both write the same value.
func BenchmarkSetDurableSmallStore(b *testing.B) {
	s, err := New(Options{Path: filepath.Join(b.TempDir(), "plugin-state.json")})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Set("ratelimiter", "counter", strconv.Itoa(i)); err != nil {
			b.Fatal(err)
		}
	}
}
