package metrics

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFeedScopeIsExactPrivateAndBounded(t *testing.T) {
	f := NewRequestFeed(2)
	f.Add(RequestEvent{ConversationID: "a", TokensIn: 1})
	f.Add(RequestEvent{ConversationID: "b", TokensIn: 2})
	f.Add(RequestEvent{ConversationID: "a", TokensIn: 3, Plugins: []string{"logger"}})
	if len(f.SnapshotForConversation("")) != 0 {
		t.Fatal("empty scope returned global feed")
	}
	a := f.SnapshotForConversation("a")
	if len(a) != 1 || a[0].TokensIn != 3 {
		t.Fatalf("scoped feed=%+v", a)
	}
	a[0].Plugins[0] = "changed"
	if f.SnapshotForConversation("a")[0].Plugins[0] != "logger" {
		t.Fatal("snapshot mutated stored plugin metadata")
	}
	raw, _ := json.Marshal(f.Snapshot())
	if strings.Contains(string(raw), "ConversationID") || strings.Contains(string(raw), "conversation_id") {
		t.Fatal("global feed exposes scope identifiers")
	}
}
