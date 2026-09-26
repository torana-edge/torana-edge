package suggest

import (
	"testing"

	"github.com/torana-edge/torana-edge/internal/pluginstate"
)

func TestRepeatedIntentKeepsConfirmationCodeAndRefreshesExpiry(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	store := New(state)
	id, err := store.Create("conversation", "router", 1, sample("strong"))
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.List("conversation", "router", 1)
	if err != nil {
		t.Fatal(err)
	}
	code := items[0].Code
	args := sample("strong")
	args.Body = "Updated cost estimate"
	id2, err := store.Create("conversation", "router", 3, args)
	if err != nil || id2 != id {
		t.Fatalf("refresh id=%s err=%v", id2, err)
	}
	items, err = store.List("conversation", "router", 4)
	if err != nil || len(items) != 1 || items[0].Status != "pending" || items[0].Code != code || items[0].Body != args.Body {
		t.Fatalf("refreshed items=%+v err=%v", items, err)
	}
	if _, err := store.ResolveCode("conversation", code, "accepted", "directive", 4); err != nil {
		t.Fatalf("original code no longer works: %v", err)
	}
}

func TestNewDedupeKeySupersedesOnlySamePluginConversation(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	store := New(state)
	for _, owner := range []struct{ conversation, plugin string }{{"a", "router"}, {"a", "other"}, {"b", "router"}} {
		if _, err := store.Create(owner.conversation, owner.plugin, 1, sample("strong")); err != nil {
			t.Fatal(err)
		}
	}
	args := sample("strong")
	args.DedupeKey = "different-intent"
	if _, err := store.Create("a", "router", 2, args); err != nil {
		t.Fatal(err)
	}
	items, err := store.List("a", "router", 2)
	if err != nil || len(items) != 2 || items[0].Status != "superseded" || items[1].Status != "pending" {
		t.Fatalf("router items=%+v err=%v", items, err)
	}
	for _, owner := range []struct{ conversation, plugin string }{{"a", "other"}, {"b", "router"}} {
		items, err := store.List(owner.conversation, owner.plugin, 2)
		if err != nil || len(items) != 1 || items[0].Status != "pending" {
			t.Fatalf("unrelated items=%+v err=%v", items, err)
		}
	}
}
