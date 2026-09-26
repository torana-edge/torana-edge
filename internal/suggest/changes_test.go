package suggest

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/pluginstate"
)

func acceptedOperation(t *testing.T, store *Store) (string, string) {
	t.Helper()
	id, err := store.CreateOperation("c", 1, OperationProposal{IntentKey: "change", IntentDigest: "digest", Title: "Confirm", Body: "Review the change.", SealedIntent: "enc:intent", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.List("c", "torana", 1)
	if err != nil {
		t.Fatal(err)
	}
	code := items[len(items)-1].Code
	if _, err := store.ResolveCode("c", code, "accepted", "directive", 1); err != nil {
		t.Fatal(err)
	}
	return id, code
}

func TestOperationChangeHistoryAndUndoSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	state, err := pluginstate.New(pluginstate.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	store := New(state)
	id, code := acceptedOperation(t, store)
	intent, err := store.PrepareOperationChange("c", id, "enc:prior-config", "candidate-revision")
	if err != nil || intent != "enc:intent" {
		t.Fatalf("prepare=%q %v", intent, err)
	}
	if _, err := store.PrepareOperationChange("c", id, "enc:prior-config", "candidate-revision"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replayed prepare: %v", err)
	}
	if _, _, err := store.ClaimUndo("c", id, "candidate-revision"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("undo before apply: %v", err)
	}
	if err := store.FinishOperation("c", id, "applied"); err != nil {
		t.Fatal(err)
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
	changes, err := store.ListChanges("c")
	if err != nil || len(changes) != 1 || changes[0].Status != "applied" {
		t.Fatalf("history=%+v %v", changes, err)
	}
	encoded, _ := json.Marshal(changes)
	for _, private := range []string{code, "prior-config", "candidate-revision"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("model history leaked %q: %s", private, encoded)
		}
	}
	if _, _, err := store.ClaimUndo("other", id, "candidate-revision"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-conversation undo: %v", err)
	}
	if _, _, err := store.ClaimUndo("c", id, "newer-revision"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale undo: %v", err)
	}
	undoID, snapshot, err := store.ClaimUndo("c", id, "candidate-revision")
	if err != nil || undoID != id || snapshot != "enc:prior-config" {
		t.Fatalf("undo=%q %q %v", undoID, snapshot, err)
	}
	if _, _, err := store.ClaimUndo("c", id, "candidate-revision"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("duplicate undo: %v", err)
	}
	if err := store.FinishUndo("c", id, "undone"); err != nil {
		t.Fatal(err)
	}
	current, _, _, _ := store.read("c")
	if current.Changes[id].SealedUndo != "" || current.Changes[id].Status != "undone" {
		t.Fatal("completed undo retained its snapshot")
	}
}

func TestUndoStorageLimitFailsBeforeExecutionClaim(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{MaxValueBytes: 2048})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	store := New(state)
	id, _ := acceptedOperation(t, store)
	if _, err := store.PrepareOperationChange("c", id, "enc:"+strings.Repeat("x", 4096), "revision"); err == nil {
		t.Fatal("oversized undo history accepted")
	}
	current, _, _, _ := store.read("c")
	if current.Operations[id].Execution != "" || len(current.Changes) != 0 {
		t.Fatal("failed storage claimed execution")
	}
	if _, err := store.PrepareOperationChange("c", id, "enc:small", "revision"); err != nil {
		t.Fatal(err)
	}
}
