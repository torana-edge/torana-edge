package suggest

import (
	"sync"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/pluginstate"
)

type countedSetupState struct {
	State
	reads int
}

func TestSetupSeenCacheRotatesAtCapacity(t *testing.T) {
	store := &Store{setupSeen: make(map[string]bool)}
	for i := range 4096 {
		store.setupSeen[time.Unix(int64(i), 0).String()] = true
	}
	store.rememberSetupSeen("new-conversation")
	if len(store.setupSeen) != 1 || !store.setupSeen["new-conversation"] {
		t.Fatal("full cache did not rotate to remember the new conversation")
	}
}

func (s *countedSetupState) GetVersioned(plugin, key string) (string, string, bool, error) {
	s.reads++
	return s.State.GetVersioned(plugin, key)
}

func TestSetupHintSteadyStateDoesNotReadStorage(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	counted := &countedSetupState{State: state}
	store := New(counted)
	now := time.Now()
	if _, err := store.SetupHint("one", "codex-thread", 1, now); err != nil {
		t.Fatal(err)
	}
	reads := counted.reads
	for range 100 {
		if _, err := store.SetupHint("two", "codex-thread", 1, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if counted.reads != reads {
		t.Fatalf("steady state made %d reads", counted.reads-reads)
	}
}

func TestSetupHintCooldownAndDismissal(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	store := New(state)
	now := time.Now().UTC()
	id, err := store.SetupHint("a", "codex-thread", 1, now)
	if err != nil || id == "" {
		t.Fatalf("first: %q %v", id, err)
	}
	if _, err := store.ResolveID("a", id, "dismissed", "agent_api", 1); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		conversation, source string
		at                   time.Time
	}{
		{"a", "codex-thread", now.Add(8 * 24 * time.Hour)},
		{"b", "codex-session", now.Add(6 * 24 * time.Hour)},
		{"c", "unknown", now},
	} {
		if id, err := New(state).SetupHint(tc.conversation, tc.source, 2, tc.at); err != nil || id != "" {
			t.Fatalf("suppressed: %q %v", id, err)
		}
	}
	if id, err := New(state).SetupHint("b", "codex-client-thread", 2, now.Add(8*24*time.Hour)); err != nil || id == "" {
		t.Fatalf("later conversation: %q %v", id, err)
	}
	items, err := store.List("b", "torana", 2)
	if err != nil || len(items) != 1 || items[0].Code != "" || len(items[0].Actions) != 1 || items[0].Actions[0].ID != "dismiss" {
		t.Fatalf("informational: %+v %v", items, err)
	}
}

func TestSetupHintConcurrentCooldown(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	store := New(state)
	now := time.Now()
	ids := make(chan string, 16)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := store.SetupHint("same", "claude-code-session", 1, now)
			if err != nil {
				t.Error(err)
			}
			if id != "" {
				ids <- id
			}
		}()
	}
	wg.Wait()
	close(ids)
	if len(ids) != 1 {
		t.Fatalf("created %d hints", len(ids))
	}
}
