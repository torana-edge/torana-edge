package conversation

import "testing"

func TestObservedUsageAccumulatesSeparatelyAndIgnoresNegativeCounters(t *testing.T) {
	r, _ := newTestRegistry(t, Options{})
	r.Observe(Observation{ID: "a", TokensIn: 10, TokensOut: 3, CacheRead: 7, CacheWrite: 2})
	r.Observe(Observation{ID: "b", TokensIn: 999})
	r.Observe(Observation{ID: "a", TokensIn: -5, TokensOut: 4, CacheRead: 5, CacheWrite: -1})
	a, _ := r.Get("a")
	if a.TokensIn != 10 || a.TokensOut != 7 || a.CacheReadTokens != 12 || a.CacheWriteTokens != 2 || a.Turns != 2 {
		t.Fatalf("usage=%+v", a)
	}
}
