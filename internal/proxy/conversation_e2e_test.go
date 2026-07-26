package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// conversationTestProxy stands up a proxy in front of a fake OpenAI-shaped
// upstream that reports prompt-cache token counts.
func conversationTestProxy(t *testing.T) (*Server, func(body string) *httptest.ResponseRecorder) {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"}}],
			"usage":{"prompt_tokens":100,"completion_tokens":5,
			         "prompt_tokens_details":{"cached_tokens":80}}}`)
	}))
	t.Cleanup(upstream.Close)

	srv, err := New(Config{
		Port: "0",
		Providers: provider.Config{
			Providers: map[string]provider.Provider{
				"oai": {URL: upstream.URL, Format: "openai"},
			},
		},
		DefaultProvider: "oai",
		ConfigPath:      filepath.Join(t.TempDir(), "config.json"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { srv.conversations.Close() })

	send := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	return srv, send
}

func chatBody(t *testing.T, system string, turns ...string) string {
	t.Helper()
	msgs := []map[string]any{{"role": "system", "content": system}}
	for i, c := range turns {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{"role": role, "content": c})
	}
	b, err := json.Marshal(map[string]any{"model": "gpt-x", "messages": msgs})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestConversationRecordedThroughRequestPath is the wiring test: a real request
// through the real pipeline must land in the registry with the provider's cache
// counts attached.
func TestConversationRecordedThroughRequestPath(t *testing.T) {
	srv, send := conversationTestProxy(t)

	if rec := send(chatBody(t, "you are helpful", "refactor the loader")); rec.Code != http.StatusOK {
		t.Fatalf("proxy status = %d: %s", rec.Code, rec.Body)
	}

	list := srv.conversations.List()
	if len(list) != 1 {
		t.Fatalf("registry holds %d conversations, want 1", len(list))
	}
	got := list[0]
	if got.ID == "" {
		t.Error("conversation was recorded without an ID")
	}
	if got.CachePrefixKey == "" {
		t.Error("cache prefix key was not recorded")
	}
	if got.Provider != "oai" || got.Model != "gpt-x" {
		t.Errorf("provider/model = %q/%q, want oai/gpt-x", got.Provider, got.Model)
	}
	if got.Path != "/v1/chat/completions" {
		t.Errorf("path = %q, want the caller's stripped path", got.Path)
	}
	if got.LastCacheRead != 80 {
		t.Errorf("cache read tokens = %d, want 80 — the provider's counts are the ground truth for warmth", got.LastCacheRead)
	}
}

// TestConversationStableAcrossTurns is the property the whole feature rests on,
// verified end to end rather than in isolation: a growing conversation stays one
// record, so anything keyed on the label does not lose track of it.
func TestConversationStableAcrossTurns(t *testing.T) {
	srv, send := conversationTestProxy(t)

	send(chatBody(t, "you are helpful", "refactor the loader"))
	send(chatBody(t, "you are helpful", "refactor the loader", "which file?", "discovery.go"))
	send(chatBody(t, "you are helpful", "refactor the loader", "which file?", "discovery.go", "done", "thanks"))

	list := srv.conversations.List()
	if len(list) != 1 {
		ids := make([]string, 0, len(list))
		for _, r := range list {
			ids = append(ids, r.ID)
		}
		t.Fatalf("three turns produced %d conversations (%v), want 1", len(list), ids)
	}
	if list[0].Turns != 3 {
		t.Errorf("Turns = %d, want 3", list[0].Turns)
	}
}

// TestDistinctConversationsStaySeparate — the converse property, so warming one
// conversation cannot be confused with another.
func TestDistinctConversationsStaySeparate(t *testing.T) {
	srv, send := conversationTestProxy(t)

	send(chatBody(t, "you are helpful", "refactor the loader"))
	send(chatBody(t, "you are helpful", "why is CI failing"))

	if got := len(srv.conversations.List()); got != 2 {
		t.Errorf("registry holds %d conversations, want 2", got)
	}
}

// TestConversationSurvivesModelSwitch — swapping models mid-conversation is a
// routing decision, not a new conversation, though the cache entry does change.
func TestConversationSurvivesModelSwitch(t *testing.T) {
	srv, send := conversationTestProxy(t)

	send(chatBody(t, "you are helpful", "refactor the loader"))
	before := srv.conversations.List()[0]

	body := `{"model":"gpt-y","messages":[{"role":"system","content":"you are helpful"},
		{"role":"user","content":"refactor the loader"}]}`
	send(body)

	list := srv.conversations.List()
	if len(list) != 1 {
		t.Fatalf("a model switch split the conversation into %d records", len(list))
	}
	if list[0].CachePrefixKey == before.CachePrefixKey {
		t.Error("cache prefix key did not change with the model — provider caches never span models")
	}
}
