package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/pluginstate"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/suggest"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func TestOperatorAcceptanceAppliesConfirmedPluginChange(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-http-server/plugin.wasm")
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	cfg := provider.DefaultConfig()
	cfg.MCP.Enabled = true
	cfg.Suggestions.Enabled = false // MCP consent does not require plugin notices.
	cfg.Plugins = provider.PluginsConfig{Dir: fixturesDir, Order: []string{"test-http-server"}, AllowUnapproved: true}
	s, err := New(Config{Port: "8080", Providers: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	registry, err := s.currentNamespaceRegistry()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := newNamespaceAccessPolicy(registry, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := operationDispatch{policy: policy, propose: s.proposeNamespaceOperation}
	result, err := dispatch.invoke(context.Background(), json.RawMessage(`{"namespace":"test-http-server","operation":"_disable"}`), plugin.MCPBinding{Bound: true, ConversationID: "operator-session", CallID: "call"})
	if err != nil || !result.OK {
		t.Fatalf("proposal=%+v %v", result, err)
	}
	items, err := s.suggestions.List("operator-session", "torana", 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v %v", items, err)
	}
	path := suggestionsAPIPath + "/" + items[0].ID + "/accept"
	accept := func(conversation string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		s.handleAgentSuggestions(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"conversation_id":"`+conversation+`"}`)))
		return recorder
	}
	if got := accept("other"); got.Code != http.StatusNotFound {
		t.Fatalf("cross-conversation=%d", got.Code)
	}
	if len(s.GetConfig().Providers.Plugins.Order) != 1 {
		t.Fatal("cross-conversation mutation")
	}
	got := accept("operator-session")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"status":"applied"`) {
		t.Fatalf("accept=%d %s", got.Code, got.Body.String())
	}
	if len(s.GetConfig().Providers.Plugins.Order) != 0 {
		t.Fatal("accepted disable was not applied")
	}
	if got := accept("operator-session"); got.Code != http.StatusNotFound {
		t.Fatalf("replay=%d %s", got.Code, got.Body.String())
	}
	changes, err := s.suggestions.ListChanges("operator-session")
	if err != nil || len(changes) != 1 || changes[0].Status != "applied" {
		t.Fatalf("history=%+v %v", changes, err)
	}
}

func TestAgentSuggestionsRequireConversationAndResolveOnce(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	store := suggest.New(state)
	if _, err := store.ObserveUserTurn("bound", "first"); err != nil {
		t.Fatal(err)
	}
	id, err := store.Create("bound", "router", 1, &pb.SuggestArgs{
		Kind: "model_switch", DedupeKey: "up", Title: "Try another model?", Body: "This might help.",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{config: Config{Providers: provider.Config{Suggestions: provider.SuggestionsConfig{Enabled: true}}}, suggestions: store}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		s.handleAgentSuggestions(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
		return recorder
	}
	if got := request(http.MethodGet, suggestionsAPIPath, ""); got.Code != http.StatusBadRequest {
		t.Fatalf("unbound list status = %d", got.Code)
	}
	if got := request(http.MethodGet, suggestionsAPIPath+"?conversation_id=other", ""); got.Code != http.StatusOK || strings.Contains(got.Body.String(), id) {
		t.Fatalf("cross-conversation list = %d %s", got.Code, got.Body.String())
	}
	path := suggestionsAPIPath + "/" + id + "/accept"
	if got := request(http.MethodPost, path, `{"conversation_id":"other"}`); got.Code != http.StatusNotFound {
		t.Fatalf("cross-conversation accept status = %d", got.Code)
	}
	if got := request(http.MethodPost, path, `{"conversation_id":"bound"}`); got.Code != http.StatusOK {
		t.Fatalf("accept status = %d: %s", got.Code, got.Body.String())
	}
	if got := request(http.MethodPost, path, `{"conversation_id":"bound"}`); got.Code != http.StatusNotFound {
		t.Fatalf("replay accept status = %d", got.Code)
	}
}
