package metrics

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestRecordPluginCounterScopesByPlugin pins the security-relevant property of
// torana_plugin_counter: the plugin name is supplied by the host, never by the
// guest payload, so one plugin cannot write into another's namespace.
func TestRecordPluginCounterScopesByPlugin(t *testing.T) {
	s := NewStatsTracker()
	s.RecordPluginCounter("pii", "requests_blocked", 2)
	s.RecordPluginCounter("pii", "requests_blocked", 3)
	s.RecordPluginCounter("otel", "requests_blocked", 7)

	snap := s.Snapshot()
	if got := snap.PluginCounters["pii"]["requests_blocked"]; got != 5 {
		t.Errorf("pii.requests_blocked = %d, want 5", got)
	}
	if got := snap.PluginCounters["otel"]["requests_blocked"]; got != 7 {
		t.Errorf("otel.requests_blocked = %d, want 7 — namespaces must not merge", got)
	}
}

// TestRecordPluginCounterIgnoresEmptyAndZero guards the no-op conditions. A
// zero delta in particular must not create an empty counter, or a plugin could
// populate /stats with names alone.
func TestRecordPluginCounterIgnoresEmptyAndZero(t *testing.T) {
	s := NewStatsTracker()
	s.RecordPluginCounter("pii", "ignored", 0)
	s.RecordPluginCounter("", "orphan", 1)
	s.RecordPluginCounter("pii", "", 1)

	if snap := s.Snapshot(); len(snap.PluginCounters) != 0 {
		t.Errorf("expected no counters recorded, got %+v", snap.PluginCounters)
	}
}

// TestSnapshotDeepCopiesCounters catches the classic bug in nested-map
// snapshots: copying the outer map but sharing the inner ones, so a caller
// holding a snapshot sees later mutations. Snapshot is what /stats serialises.
func TestSnapshotDeepCopiesCounters(t *testing.T) {
	s := NewStatsTracker()
	s.RecordPluginCounter("pii", "blocked", 1)

	snap := s.Snapshot()
	s.RecordPluginCounter("pii", "blocked", 99)
	s.RecordPluginCounter("pii", "new_counter", 1)

	if got := snap.PluginCounters["pii"]["blocked"]; got != 1 {
		t.Errorf("snapshot mutated after the fact: blocked = %d, want 1", got)
	}
	if _, leaked := snap.PluginCounters["pii"]["new_counter"]; leaked {
		t.Error("a counter added after Snapshot appeared in the snapshot")
	}
}

// TestRecordPluginCounterConcurrent runs under -race in CI. The counter map is
// mutex-guarded while the rest of StatsTracker uses atomics, so the mixed
// discipline is worth pinning.
func TestRecordPluginCounterConcurrent(t *testing.T) {
	s := NewStatsTracker()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.RecordPluginCounter("pii", "hits", 1)
			_ = s.Snapshot()
		}()
	}
	wg.Wait()

	if got := s.Snapshot().PluginCounters["pii"]["hits"]; got != 50 {
		t.Errorf("hits = %d, want 50", got)
	}
}

func TestPluginCounterNamesAndCardinalityAreBounded(t *testing.T) {
	s := NewStatsTracker()
	for i := range maxPluginCounterNames + 10_000 {
		s.RecordPluginCounter("attacker", fmt.Sprintf("counter-%02d", i), 1)
	}
	// Existing names remain usable at the cap. Every new, malformed, or
	// reserved name is represented by one fixed host-owned overflow key.
	s.RecordPluginCounter("attacker", "counter-00", 4)
	for _, name := range []string{
		" space",
		"counter/with/slashes",
		strings.Repeat("z", 65),
		pluginCounterOverflow,
	} {
		s.RecordPluginCounter("attacker", name, 99)
	}

	counters := s.Snapshot().PluginCounters["attacker"]
	if got, want := len(counters), maxPluginCounterNames+1; got != want {
		t.Fatalf("counter map grew to %d entries, want bounded %d", got, want)
	}
	if got := counters["counter-00"]; got != 5 {
		t.Fatalf("existing counter at cap = %d, want 5", got)
	}
	if got, want := counters[pluginCounterOverflow], int64(10_004); got != want {
		t.Fatalf("overflow attempts = %d, want %d", got, want)
	}
}

func TestPluginTelemetryNameGrammar(t *testing.T) {
	valid := []string{"a", "0", "requests_total", "cache.hit-rate", "a" + strings.Repeat("z", 63)}
	for _, name := range valid {
		if !validPluginTelemetryName(name) {
			t.Errorf("valid name %q rejected", name)
		}
	}
	invalid := []string{"", "_reserved", "-leading", ".leading", "space name", "slash/name", "Kelvin", "a\x00b", strings.Repeat("a", 65)}
	for _, name := range invalid {
		if validPluginTelemetryName(name) {
			t.Errorf("invalid name %q accepted", name)
		}
	}
}
