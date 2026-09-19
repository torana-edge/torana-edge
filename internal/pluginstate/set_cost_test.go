package pluginstate

import (
	"path/filepath"
	"strconv"
	"testing"
)

// These benchmarks keep the request-path cost of persistent state visible and
// compare a warm 500-key store with a nearly empty one. A single-key bbolt
// transaction should not scale linearly with the total resident key count.

func seedStore(tb testing.TB, s *Store, plugin string, n int) {
	tb.Helper()
	for i := 0; i < n; i++ {
		if err := s.Set(plugin, "prefix"+strconv.Itoa(i), "some stored prefix value"); err != nil {
			tb.Fatal(err)
		}
	}
}

// BenchmarkSetDurable is one bbolt transaction against 500 resident keys.
func BenchmarkSetDurable(b *testing.B) {
	s, err := New(Options{Path: filepath.Join(b.TempDir(), "plugin-state.db")})
	if err != nil {
		b.Fatal(err)
	}
	seedStore(b, s, "ratelimiter", 500)
	b.Cleanup(func() { _ = s.Close() })
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Set("ratelimiter", "new-key", strconv.Itoa(i)); err != nil {
			b.Fatal(err)
		}
		if err := s.Delete("ratelimiter", "new-key"); err != nil {
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
	seedStore(b, s, "ratelimiter", 500)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Set("ratelimiter", "new-key", strconv.Itoa(i)); err != nil {
			b.Fatal(err)
		}
		if err := s.Delete("ratelimiter", "new-key"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSetDurableSmallStore is the same durable write against a nearly
// empty store. Comparing it with BenchmarkSetDurable catches a regression to
// whole-store work — both write the same value.
func BenchmarkSetDurableSmallStore(b *testing.B) {
	s, err := New(Options{Path: filepath.Join(b.TempDir(), "plugin-state.db")})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Set("ratelimiter", "new-key", strconv.Itoa(i)); err != nil {
			b.Fatal(err)
		}
		if err := s.Delete("ratelimiter", "new-key"); err != nil {
			b.Fatal(err)
		}
	}
}
