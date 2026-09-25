package suggest

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/torana-edge/torana-edge/internal/pluginstate"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func sample(model string) *pb.SuggestArgs {
	return &pb.SuggestArgs{
		Kind: "model_switch", DedupeKey: "upgrade", Title: "Try a stronger model?",
		Body: "The last tool calls failed.", HarnessTargetModel: &model,
		Actions: []*pb.SuggestAction{{Id: "accept", Label: "Switch"}},
	}
}

func TestSuggestionLifecycleAndConversationScope(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	store := New(state)
	firstID, err := store.Create("conversation-a", "router", 1, sample("strong"))
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.List("conversation-a", "router", 1)
	if err != nil || len(items) != 1 || items[0].ID != firstID || items[0].Status != "pending" {
		t.Fatalf("first suggestion: %+v, %v", items, err)
	}
	firstCode := items[0].Code
	if len(firstCode) != 4 {
		t.Fatalf("confirmation code = %q", firstCode)
	}
	if _, err := store.ResolveCode("conversation-b", firstCode, "accepted", "directive", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-conversation code resolved: %v", err)
	}
	secondID, err := store.Create("conversation-a", "router", 2, sample("strong"))
	if err != nil || secondID == firstID {
		t.Fatalf("replacement suggestion: %q, %v", secondID, err)
	}
	items, err = store.List("conversation-a", "router", 2)
	if err != nil || len(items) != 2 || items[0].Status != "superseded" || items[1].Status != "pending" || items[1].Code == firstCode {
		t.Fatalf("supersede: %+v, %v", items, err)
	}
	if _, err := store.ResolveCode("conversation-a", firstCode, "accepted", "directive", 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("superseded code resolved: %v", err)
	}
	accepted, err := store.ResolveCode("conversation-a", items[1].Code, "accepted", "directive", 2)
	if err != nil || accepted.Status != "accepted" || accepted.Via != "directive" {
		t.Fatalf("accept: %+v, %v", accepted, err)
	}
	if _, err := store.ResolveCode("conversation-a", items[1].Code, "accepted", "directive", 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("single-use code reused: %v", err)
	}
}

func TestSuggestionExpiryAndHarnessSwitch(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	store := New(state)
	if _, err := store.Create("c", "router", 4, sample("strong")); err != nil {
		t.Fatal(err)
	}
	accepted, err := store.AcceptHarnessSwitch("c", "other", 5)
	if err != nil || len(accepted) != 0 {
		t.Fatalf("wrong model accepted: %+v, %v", accepted, err)
	}
	accepted, err = store.AcceptHarnessSwitch("c", "strong", 5)
	if err != nil || len(accepted) != 1 || accepted[0].Via != "harness_switch" {
		t.Fatalf("harness switch: %+v, %v", accepted, err)
	}
	if _, err := store.Create("c", "router", 5, sample("strong")); err != nil {
		t.Fatal(err)
	}
	items, err := store.List("c", "router", 8)
	if err != nil || items[1].Status != "expired" {
		t.Fatalf("expiry: %+v, %v", items, err)
	}
}

func TestSuggestionStateSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin-state.db")
	state, err := pluginstate.New(pluginstate.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	store := New(state)
	id, err := store.Create("resumed", "router", 3, sample("strong"))
	if err != nil {
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
	items, err := New(state).List("resumed", "router", 4)
	if err != nil || len(items) != 1 || items[0].ID != id {
		t.Fatalf("restarted suggestion: %+v, %v", items, err)
	}
}
