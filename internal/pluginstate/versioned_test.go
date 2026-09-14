package pluginstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentCASOnlyOneWinner(t *testing.T) {
	s := newStore(t, Options{})
	var wg sync.WaitGroup
	var wins int
	var mu sync.Mutex
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _, err := s.CompareAndSet("p", "k", "v", nil)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("CAS winners=%d", wins)
	}
}

func TestVersionsSurviveRestartAndABA(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	s := newStore(t, Options{Path: p})
	ok, v1, e := s.CompareAndSet("p", "k", "a", nil)
	if e != nil || !ok {
		t.Fatal(e)
	}
	s2 := newStore(t, Options{Path: p})
	_, v, ok := s2.GetVersioned("p", "k")
	if !ok || v != v1 {
		t.Fatalf("version %q want %q", v, v1)
	}
	if ok, e := s2.CompareAndDelete("p", "k", v1); e != nil || !ok {
		t.Fatal(ok, e)
	}
	ok, v2, e := s2.CompareAndSet("p", "k", "b", nil)
	if e != nil || !ok || v2 == v1 {
		t.Fatalf("recreate %v %q", ok, v2)
	}
	if ok, _, _ := s2.CompareAndSet("p", "k", "c", &v1); ok {
		t.Fatal("stale CAS succeeded")
	}
}

func TestScanBudgetAndCursorValidation(t *testing.T) {
	s := newStore(t, Options{})
	_ = s.Set("p", "a", "123456789")
	if _, _, e := s.Scan("p", "", "", 1, 2); e == nil {
		t.Fatal("oversized first entry accepted")
	}
	if e := s.Set("p", "b", "v"); e != nil {
		t.Fatal(e)
	}
	_, cur, e := s.Scan("p", "", "", 1, 100)
	if e != nil || cur == "" {
		t.Fatalf("scan cursor %q %v", cur, e)
	}
	if _, _, e = s.Scan("q", "", cur, 1, 100); e == nil {
		t.Fatal("invalid namespace cursor accepted")
	}
}

func TestLegacyUpgradeStable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if e := os.WriteFile(p, []byte(`{"p":{"k":"v"}}`), 0600); e != nil {
		t.Fatal(e)
	}
	s := newStore(t, Options{Path: p})
	_, v1, ok := s.GetVersioned("p", "k")
	if !ok || v1 == "" {
		t.Fatal("missing migrated version")
	}
	s2 := newStore(t, Options{Path: p})
	_, v2, _ := s2.GetVersioned("p", "k")
	if v1 != v2 {
		t.Fatalf("version changed %q %q", v1, v2)
	}
}

func TestInvalidEnvelopeRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	env := stateEnvelope{Format: 1, Counter: 1, Data: map[string]map[string]PageEntry{"p": {"k": {Key: "k", Value: "v", Version: "2"}}}}
	raw, _ := json.Marshal(env)
	_ = os.WriteFile(p, raw, 0600)
	s, e := New(Options{Path: p})
	if e == nil || s == nil {
		t.Fatal("invalid envelope not rejected")
	}
	if e := s.Set("p", "x", "y"); e == nil {
		t.Fatal("invalid store writable")
	}
}

func TestCounterExhausted(t *testing.T) {
	s := newStore(t, Options{})
	s.counter = ^uint64(0)
	if _, _, e := s.CompareAndSet("p", "k", "v", nil); e != ErrVersionExhausted {
		t.Fatalf("err=%v", e)
	}
}
