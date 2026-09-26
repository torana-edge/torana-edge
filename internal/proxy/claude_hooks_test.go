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
	defer s.mcpLimits.Close()
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
	if got := request("stop", body, token); strings.TrimSpace(got.Body.String()) != "{}" {
		t.Fatal("repeated Stop announced the same suggestion")
	}
	other := request("stop", strings.ReplaceAll(body, `"session"`, `"other"`), token)
	if strings.TrimSpace(other.Body.String()) != "{}" {
		t.Fatalf("cross-session suggestion: %s", other.Body.String())
	}
	switchBody := `{"session_id":"session","hook_event_name":"PostModelSwitch","to_model":"strong","source":"resume"}`
	for _, source := range []string{"auto", "resume"} {
		if got := request("post-model-switch", strings.ReplaceAll(switchBody, `"resume"`, `"`+source+`"`), token); got.Code != http.StatusOK {
			t.Fatal(got.Body.String())
		}
		items, err := s.suggestions.List(conversation, "router", 0)
		if err != nil || len(items) != 1 || items[0].Status != "pending" {
			t.Fatalf("automatic observation accepted advice: %+v %v", items, err)
		}
	}
	if got := request("post-model-switch", strings.ReplaceAll(switchBody, `"resume"`, `"command"`), token); got.Code != http.StatusOK {
		t.Fatal(got.Body.String())
	}
	items, err := s.suggestions.List(conversation, "router", 0)
	if err != nil || len(items) != 1 || items[0].Status != "accepted" || items[0].Via != "adapter" {
		t.Fatalf("switch=%+v %v", items, err)
	}
	if request("stop", body+body, token).Code != http.StatusBadRequest {
		t.Fatal("trailing hook payload accepted")
	}
	pre := `{"session_id":"session","hook_event_name":"PreModelSwitch","to_model":"strong","source":"command","context_tokens":12345,"prompt_cache_warm":true,"estimated_cache_write_usd":0.1234}`
	assertSilent := func(response *httptest.ResponseRecorder) {
		t.Helper()
		if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "{}" {
			t.Fatalf("pre-switch error did not fail open: %d %s", response.Code, response.Body.String())
		}
	}
	assertSilent(request("pre-model-switch", pre, token))
	s.config.Providers.Suggestions.ClaudeCode.PreModelSwitch = true
	// No conversation reads or writes are needed to report the supplied estimate.
	s.suggestions = nil
	response = request("pre-model-switch", pre, token)
	result = nil
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result) != 1 || !strings.Contains(response.Body.String(), "12345") || !strings.Contains(response.Body.String(), "$0.1234") {
		t.Fatalf("warning=%s", response.Body.String())
	}
	for _, source := range []string{"picker", "sdk"} {
		if request("pre-model-switch", strings.ReplaceAll(pre, `"command"`, `"`+source+`"`), token).Code != http.StatusOK {
			t.Fatalf("valid source rejected: %s", source)
		}
	}
	if got := request("pre-model-switch", strings.ReplaceAll(pre, `"prompt_cache_warm":true`, `"prompt_cache_warm":false`), token); strings.TrimSpace(got.Body.String()) != "{}" {
		t.Fatal("cold cache produced a warning")
	}
	assertSilent(request("pre-model-switch", strings.ReplaceAll(pre, "0.1234", "-1"), token))
	assertSilent(request("pre-model-switch", strings.ReplaceAll(pre, `"command"`, `"auto"`), token))
	assertSilent(request("pre-model-switch", "{", token))
	assertSilent(request("pre-model-switch", pre, "wrong"))
	s.config.Providers.MCP.Enabled = false
	assertSilent(request("pre-model-switch", pre, token))
	s.config.Providers.MCP.Enabled = true
	s.mcpLimits.Update(120, 1)
	release, allowed := s.mcpLimits.acquireLease("hook:claude-code:pre-model-switch")
	if !allowed {
		t.Fatal("could not occupy the hook lease")
	}
	assertSilent(request("pre-model-switch", pre, token))
	release()
	if _, err := s.mcpTokens.Rotate(); err != nil {
		t.Fatal(err)
	}
	if request("stop", body, token).Code != http.StatusUnauthorized {
		t.Fatal("rotated token remained valid")
	}
	assertSilent(request("pre-model-switch", pre, token))
}
