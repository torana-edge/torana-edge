package proxy

import (
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/directive"
	"github.com/torana-edge/torana-edge/internal/pluginstate"
	"github.com/torana-edge/torana-edge/internal/suggest"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func TestDispatchCoreDirectivesBoundToConversationAndSingleUse(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	store := suggest.New(state)
	turn, err := store.ObserveUserTurn("conversation", "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("conversation", "router", turn, &pb.SuggestArgs{Kind: "model_switch", DedupeKey: "up", Title: "Try Opus", Body: "A stronger model may help."}); err != nil {
		t.Fatal(err)
	}
	items, err := store.List("conversation", "", turn)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	command := directive.Command{Known: true, Namespace: "torana", Verb: "accept", Args: items[0].Code}
	if got := dispatchCoreDirectives(store, "other", turn, []directive.Command{command}); !strings.Contains(got, "not pending") {
		t.Fatalf("cross-conversation accepted: %s", got)
	}
	if got := dispatchCoreDirectives(store, "conversation", turn, []directive.Command{command}); !strings.Contains(got, "Accepted: Try Opus") {
		t.Fatalf("accept failed: %s", got)
	}
	if got := dispatchCoreDirectives(store, "conversation", turn, []directive.Command{command}); !strings.Contains(got, "not pending") {
		t.Fatalf("replay accepted: %s", got)
	}
}
