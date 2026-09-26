package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/mcpauth"
	"github.com/torana-edge/torana-edge/internal/pluginstate"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/secret"
	"github.com/torana-edge/torana-edge/internal/suggest"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// Explicit opt-in: two bounded Haiku requests use the locally logged-in CLI.
// No user settings or Torana daemon are modified and no tool access is granted.
func TestLiveClaudeHookStopAndResume(t *testing.T) {
	if os.Getenv("TORANA_LIVE_CLAUDE_HOOKS") != "1" {
		t.Skip("opt-in live Claude CLI check")
	}
	binary, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal("Claude CLI is required for this opt-in check")
	}
	state, err := pluginstate.New(pluginstate.Options{Path: t.TempDir() + "/state.db"})
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
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	id[6], id[8] = id[6]&15|64, id[8]&63|128
	session := fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
	conversation := engine.ExternalConversationID("claude-code-session", session)
	model := "fixture-never-selected"
	if _, err := s.suggestions.Create(conversation, "router", 0, &pb.SuggestArgs{Kind: "model_switch", DedupeKey: "live-hook", Title: "Live check", Body: "Live check", HarnessTargetModel: &model}); err != nil {
		t.Fatal(err)
	}
	var stops, resumes atomic.Int32
	var lastSwitchSource atomic.Value
	lastSwitchSource.Store("")
	guarded := s.controlPlaneGuard(s.handleClaudeHook)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var captured bytes.Buffer
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.TeeReader(r.Body, &captured), r.Body}
		response := httptest.NewRecorder()
		guarded(response, r)
		for key, values := range response.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
		if response.Code != http.StatusOK {
			return
		}
		if strings.HasSuffix(r.URL.Path, "/stop") {
			stops.Add(1)
		} else if strings.HasSuffix(r.URL.Path, "/post-model-switch") {
			var event struct {
				Source string `json:"source"`
			}
			if json.Unmarshal(captured.Bytes(), &event) == nil {
				lastSwitchSource.Store(event.Source)
			}
			resumes.Add(1)
		}
	}))
	defer server.Close()
	hooks := map[string]any{}
	for event, path := range map[string]string{"Stop": "stop", "PostModelSwitch": "post-model-switch"} {
		hooks[event] = []any{map[string]any{"hooks": []any{map[string]any{
			"type": "http", "url": server.URL + claudeHooksPath + path, "timeout": 3,
			"headers":        map[string]string{"Authorization": "Bearer $TORANA_MCP_TOKEN", "X-Torana-Local-Request": "1"},
			"allowedEnvVars": []string{"TORANA_MCP_TOKEN"},
		}}}}
	}
	settings, _ := json.Marshal(map[string]any{"hooks": hooks})
	project := t.TempDir()
	run := func(resume bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		args := []string{"-p", "--tools", "", "--setting-sources", "", "--settings", string(settings), "--output-format", "json"}
		if resume {
			args = append(args, "--resume", session)
		} else {
			args = append(args, "--session-id", session, "--model", "haiku")
		}
		args = append(args, "Reply with only hook-check. Do not use tools.")
		command := exec.CommandContext(ctx, binary, args...)
		command.Dir = project
		for _, item := range os.Environ() {
			if !strings.HasPrefix(item, "TORANA_MCP_TOKEN=") {
				command.Env = append(command.Env, item)
			}
		}
		command.Env = append(command.Env, "TORANA_MCP_TOKEN="+token)
		// Captured output stays private: CLI diagnostics can contain local paths.
		if _, err := command.CombinedOutput(); err != nil {
			t.Fatalf("bounded Claude invocation failed: %v (output kept private)", err)
		}
	}
	run(false)
	items, err := s.suggestions.List(conversation, "router", 0)
	if err != nil || stops.Load() == 0 || len(items) != 1 || !items[0].HookAnnounced {
		t.Fatalf("Stop did not deliver the session-bound announcement: calls=%d error=%v", stops.Load(), err)
	}
	beforeResume := resumes.Load()
	run(true)
	if resumes.Load() <= beforeResume {
		t.Fatal("resume did not emit PostModelSwitch")
	}
	if lastSwitchSource.Load() != "resume" {
		t.Fatalf("restore callback source=%s", lastSwitchSource.Load())
	}
	items, err = s.suggestions.List(conversation, "router", 0)
	if err != nil || len(items) != 1 || items[0].Status != "pending" {
		t.Fatal("resume incorrectly accepted model advice")
	}
}
