package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/mcpauth"
	"github.com/torana-edge/torana-edge/internal/pluginstate"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/secret"
	"github.com/torana-edge/torana-edge/internal/suggest"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func TestClaudeHooksGuardIdentityAndNonBlockingSwitchObservation(t *testing.T) {
	state, err := pluginstate.New(pluginstate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	sealer, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := provider.DefaultConfig()
	cfg.MCP.Enabled = true
	cfg.Suggestions.ClaudeCode.Enabled = true
	s := &Server{config: Config{Providers: cfg}, mcpTokens: mcpauth.New(state, sealer), suggestions: suggest.New(state), mcpLimits: NewRateLimiter(120, 8)}
	token, err := s.mcpTokens.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	conversation := engine.ExternalConversationID("claude-code-session", "session")
	model := "strong"
	if _, err := s.suggestions.Create(conversation, "router", 0, &pb.SuggestArgs{Kind: "model_switch", DedupeKey: "up", Title: "private-secret", Body: "private-secret", HarnessTargetModel: &model}); err != nil {
		t.Fatal(err)
	}
	request := func(path, event, bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080"+claudeHooksPath+path, strings.NewReader(event))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("X-Torana-Local-Request", "1")
		r.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		s.controlPlaneGuard(s.handleClaudeHook)(w, r)
		return w
	}
	body := `{"session_id":"session","hook_event_name":"Stop","transcript_path":"/never/read/this","cwd":"/ignored"}`
	if request("stop", body, "wrong").Code != http.StatusUnauthorized {
		t.Fatal("unauthenticated hook accepted")
	}
	response := request("stop", body, token)
	var result map[string]any
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result["systemMessage"] == nil || len(result) != 1 {
		t.Fatalf("stop=%s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "private-secret") {
		t.Fatal("guest text leaked")
	}
	other := request("stop", strings.ReplaceAll(body, `"session"`, `"other"`), token)
	if strings.TrimSpace(other.Body.String()) != "{}" {
		t.Fatalf("cross-session suggestion: %s", other.Body.String())
	}
	switchBody := `{"session_id":"session","hook_event_name":"PostModelSwitch","to_model":"strong","source":"resume"}`
	if got := request("post-model-switch", switchBody, token); got.Code != http.StatusOK {
		t.Fatal(got.Body.String())
	}
	items, err := s.suggestions.List(conversation, "router", 0)
	if err != nil || len(items) != 1 || items[0].Status != "accepted" || items[0].Via != "adapter" {
		t.Fatalf("switch=%+v %v", items, err)
	}
	if request("stop", body+body, token).Code != http.StatusBadRequest {
		t.Fatal("trailing hook payload accepted")
	}
	if _, err := s.mcpTokens.Rotate(); err != nil {
		t.Fatal(err)
	}
	if request("stop", body, token).Code != http.StatusUnauthorized {
		t.Fatal("rotated token remained valid")
	}
}
