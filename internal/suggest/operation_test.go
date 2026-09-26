package suggest

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/pluginstate"
)

func TestOperationConsentSurvivesRestartAndClaimsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	state, err := pluginstate.New(pluginstate.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	store := New(state)
	proposal := OperationProposal{IntentKey: "intent-1", IntentDigest: "digest-1", Title: "Enable plugin?", Body: "Enable the selected plugin.", SealedIntent: "enc:private-intent", ExpiresAt: time.Now().Add(time.Hour)}
	id, err := store.CreateOperation("c", 1, proposal)
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.List("c", "torana", 1000)
	if err != nil || len(items) != 1 || items[0].Status != "pending" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	code := items[0].Code
	encoded, err := json.Marshal(items)
	if err != nil || strings.Contains(string(encoded), proposal.SealedIntent) {
		t.Fatalf("private payload exposed: %s, %v", encoded, err)
	}
	if _, err := store.ClaimOperation("c", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim before consent: %v", err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = pluginstate.New(pluginstate.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	store = New(state)
	if _, err := store.ResolveCode("c", code, "accepted", "directive", 1001); err != nil {
		t.Fatal(err)
	}
	var claims atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			intent, err := store.ClaimOperation("c", id)
			if err == nil {
				if intent != proposal.SealedIntent {
					t.Errorf("intent=%q", intent)
				}
				claims.Add(1)
			} else if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrConflict) {
				t.Errorf("claim: %v", err)
			}
		}()
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatalf("claims=%d", claims.Load())
	}
	if err := store.FinishOperation("c", id, "applied"); err != nil {
		t.Fatal(err)
	}
	current, _, _, err := store.read("c")
	if err != nil || len(current.Operations) != 0 || current.Suggestions[0].Outcome != "applied" || current.Suggestions[0].Action != "accepted" {
		t.Fatalf("finish=%+v, %v", current, err)
	}
	if _, err := store.ClaimOperation("c", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replayed claim: %v", err)
	}
}

func TestOperationRefreshSupersessionAndExpiry(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	store := New(state)
	proposal := OperationProposal{IntentKey: "a", IntentDigest: "digest-a", Title: "Apply?", Body: "Confirm the change.", SealedIntent: "enc:a", ExpiresAt: time.Now().Add(time.Hour)}
	id, err := store.CreateOperation("c", 1, proposal)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := store.List("c", "torana", 1)
	code := items[0].Code
	refresh, err := store.CreateOperation("c", 2, proposal)
	if err != nil || refresh != id {
		t.Fatalf("refresh=%q err=%v", refresh, err)
	}
	items, _ = store.List("c", "torana", 2)
	if items[0].Code != code {
		t.Fatal("refresh changed confirmation code")
	}
	proposal.IntentDigest, proposal.SealedIntent = "digest-b", "enc:b"
	second, err := store.CreateOperation("c", 3, proposal)
	if err != nil || second == id {
		t.Fatalf("replacement=%q err=%v", second, err)
	}
	current, _, _, _ := store.read("c")
	if _, exists := current.Operations[id]; exists {
		t.Fatal("superseded intent retained")
	}
	items, _ = store.List("c", "torana", 3)
	if items[1].Code == code {
		t.Fatal("changed digest reused confirmation code")
	}
	if _, err := store.ResolveCode("c", code, "accepted", "directive", 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old code accepted changed intent: %v", err)
	}
	if _, err := store.ResolveID("c", second, "accepted", "directive", 3); err != nil {
		t.Fatal(err)
	}
	if err := store.update("c", func(r *record) (bool, error) {
		op := r.Operations[second]
		op.ExpiresAt = time.Now().Add(-time.Second)
		r.Operations[second] = op
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveID("c", second, "accepted", "directive", 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired consent accepted: %v", err)
	}
	current, _, _, _ = store.read("c")
	if _, exists := current.Operations[second]; exists || current.Suggestions[1].Status != "expired" {
		t.Fatal("accepted but unclaimed expired intent retained")
	}
	args := sample("strong")
	args.Kind = "torana_operation"
	if _, err := store.Create("c", "guest", 3, args); err == nil {
		t.Fatal("guest forged host kind")
	}
	args.Kind = "model_switch"
	if _, err := store.Create("c", "torana", 3, args); err == nil {
		t.Fatal("guest forged host source")
	}
}

func TestIndependentHostProposalsRemainPending(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	store := New(state)
	for _, key := range []string{"enable-logger", "configure-compactor"} {
		if _, err := store.CreateOperation("c", 1, OperationProposal{IntentKey: key, IntentDigest: key, Title: "Confirm", Body: "Review the change.", SealedIntent: "enc:" + key, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.List("c", "torana", 1)
	if err != nil || len(items) != 2 || items[0].Status != "pending" || items[1].Status != "pending" {
		t.Fatalf("items=%+v %v", items, err)
	}
}
