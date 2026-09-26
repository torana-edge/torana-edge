package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/torana-edge/torana-edge/internal/auditlog"
	"github.com/torana-edge/torana-edge/internal/provider"
)

type mcpTestTransport struct{ token string }

func (t mcpTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(r)
}

func newMCPTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := provider.DefaultConfig()
	cfg.Plugins.Dir = t.TempDir()
	s, err := New(Config{HostVersion: "test", Providers: cfg, ConfigPath: filepath.Join(t.TempDir(), "config.json")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestMountedMCPDefaultOffAndAuthentication(t *testing.T) {
	s := newMCPTestServer(t)
	request := func(token, host, remote string) int {
		r := httptest.NewRequest(http.MethodPost, "http://localhost/_torana/mcp", strings.NewReader(`{}`))
		r.Host, r.RemoteAddr = host, remote
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w.Code
	}
	if got := request("", "localhost", "127.0.0.1:1234"); got != 404 {
		t.Fatalf("disabled status=%d", got)
	}
	cfg := s.GetConfig().Providers
	cfg.MCP.Enabled = true
	if err := s.SetProviders(cfg); err != nil {
		t.Fatal(err)
	}
	if got := request("", "localhost", "127.0.0.1:1234"); got != 401 {
		t.Fatalf("missing token status=%d", got)
	}
	if token, err := s.mcpTokens.Current(); err != nil || token != "" {
		t.Fatal("authentication request provisioned token")
	}
	token, err := s.mcpTokens.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token, host, remote string
		want                int
	}{
		{"wrong", "localhost", "127.0.0.1:1234", 401},
		{token, "foreign.example", "127.0.0.1:1234", 403},
		{token, "localhost", "192.0.2.1:1234", 403},
	} {
		if got := request(tc.token, tc.host, tc.remote); got != tc.want {
			t.Fatalf("auth guard status=%d want=%d", got, tc.want)
		}
	}
	if _, err := s.mcpTokens.Rotate(); err != nil {
		t.Fatal(err)
	}
	if got := request(token, "localhost", "127.0.0.1:1234"); got != 401 {
		t.Fatalf("old token status=%d", got)
	}
}

func TestMountedMCPOfficialClientAndLiveDisable(t *testing.T) {
	s := newMCPTestServer(t)
	cfg := s.GetConfig().Providers
	cfg.MCP.Enabled = true
	if err := s.SetProviders(cfg); err != nil {
		t.Fatal(err)
	}
	token, err := s.mcpTokens.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connect := func() *mcp.ClientSession {
		client := mcp.NewClient(&mcp.Implementation{Name: "host-test", Version: "test"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/_torana/mcp", HTTPClient: &http.Client{Transport: mcpTestTransport{token: token}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = session.Close() })
		return session
	}
	session := connect()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 4 {
		t.Fatal("fixed tool discovery failed")
	}
	for _, tc := range []struct {
		tool   string
		args   map[string]any
		denied bool
	}{
		{"torana_namespaces", map[string]any{}, false},
		{"torana_describe", map[string]any{"namespace": "torana"}, false},
		{"torana_search", map[string]any{"query": "status"}, false},
		{"torana_invoke", map[string]any{"namespace": "torana", "operation": "system.status"}, false},
		{"torana_invoke", map[string]any{"namespace": "torana", "operation": "system.stop"}, true},
		{"torana_invoke", map[string]any{"namespace": "torana", "operation": "changes.undo", "input": map[string]any{"code": "secret-must-not-echo"}}, true},
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
		if err != nil || result.IsError != tc.denied {
			t.Fatalf("tool %s failed expected outcome", tc.tool)
		}
		encoded, err := json.Marshal(result)
		if err != nil || strings.Contains(string(encoded), "secret-must-not-echo") {
			t.Fatal("model response echoed operator input")
		}
	}
	s.mcpMu.Lock()
	old := s.mcpHandler
	s.mcpMu.Unlock()
	cfg.MCP.Enabled = false
	if err := s.SetProviders(cfg); err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.Done():
	case <-ctx.Done():
		t.Fatal("disable left MCP sessions running")
	}
	if _, err := session.ListTools(ctx, nil); err == nil {
		t.Fatal("disabled MCP still serves sessions")
	}
	cfg.MCP.Enabled = true
	if err := s.SetProviders(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := connect().ListTools(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.shutdownMCP(ctx); err != nil {
		t.Fatal(err)
	}
	if result, err := s.dispatchMCP(ctx, "torana_namespaces", json.RawMessage(`{}`)); err != nil || result.Error == nil || result.Error.Code != "not_configured" {
		t.Fatal("stopped dispatcher admitted a call")
	}
	// Admission stays closed even when configuration still says enabled.
	w := httptest.NewRecorder()
	s.handleMCP(w, httptest.NewRequest("POST", "http://localhost/_torana/mcp", nil))
	if w.Code != 404 {
		t.Fatalf("stopped status=%d", w.Code)
	}
}

func TestMCPPolicyCacheRefreshesOnOperatorChange(t *testing.T) {
	s := newMCPTestServer(t)
	first, err := s.mcpPolicy()
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.mcpPolicy()
	if err != nil || first != second {
		t.Fatal("unchanged catalog was not cached")
	}
	cfg := s.GetConfig().Providers
	cfg.MCP.Access = map[string]string{"torana.system.status": "never"}
	if err := s.SetProviders(cfg); err != nil {
		t.Fatal(err)
	}
	third, err := s.mcpPolicy()
	if err != nil || third == first || third.ModelReachable("torana", "system.status") != "never" {
		t.Fatal("changed policy reused a stale catalog")
	}
}

func TestMCPRateLimitReturnsSafeRetry(t *testing.T) {
	s := newMCPTestServer(t)
	s.mcpLimits.Update(1, 1)
	cfg := s.GetConfig().Providers
	cfg.MCP.Enabled = true
	if err := s.SetProviders(cfg); err != nil {
		t.Fatal(err)
	}
	first, err := s.dispatchMCP(context.Background(), "torana_namespaces", json.RawMessage(`{}`))
	if err != nil || !first.OK {
		t.Fatal("first call denied")
	}
	second, err := s.dispatchMCP(context.Background(), "torana_namespaces", json.RawMessage(`{}`))
	if err != nil || second.Error == nil || second.Error.Code != "rate_limited" || !second.Error.Retryable {
		t.Fatal("rate limit did not return a safe retry outcome")
	}
}

func TestMCPAuditContainsOnlySafeMetadata(t *testing.T) {
	s := newMCPTestServer(t)
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	writer, err := auditlog.Open(auditlog.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	s.swapAuditWriter(writer)
	cfg := s.GetConfig().Providers
	cfg.MCP.Enabled = true
	if err := s.SetProviders(cfg); err != nil {
		t.Fatal(err)
	}
	_, err = s.dispatchMCP(context.Background(), "torana_invoke", json.RawMessage(`{"namespace":"torana","operation":"changes.undo","input":{"code":"private-sentinel"}}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.dispatchMCP(context.Background(), "private-sentinel", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	s.swapAuditWriter(nil)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-sentinel") || !strings.Contains(string(data), `"type":"mcp_call"`) || !strings.Contains(string(data), `"tool":"unknown"`) {
		t.Fatal("audit did not enforce safe metadata")
	}
}
