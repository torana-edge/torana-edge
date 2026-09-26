package controlcmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/proxy"
)

func TestMCPInvalidCommandsNeverContactServer(t *testing.T) {
	for _, args := range [][]string{
		{"mcp", "enable"}, {"mcp", "disable"}, {"mcp", "rotate"},
		{"mcp", "token", "extra"}, {"mcp", "token", "--file", "-"},
		{"mcp", "token", "--json", "--json"}, {"mcp", "unknown"},
		{"mcp", "stdio", "--json"}, {"mcp", "stdio", "--yes"},
	} {
		out, _, err := invoke("127.0.0.1:1", "", args...)
		if err == nil || out != "" || strings.Contains(err.Error(), "could not reach") {
			t.Fatalf("invalid command was not rejected: %v", args)
		}
	}
	if !Handles([]string{"mcp", "token"}) {
		t.Fatal("MCP commands not routed")
	}
}

func TestMCPStdioCLIRealHost(t *testing.T) {
	cfg := provider.DefaultConfig()
	cfg.Plugins.Dir = t.TempDir()
	cfg.MCP.Enabled = true
	s, err := proxy.New(proxy.Config{Providers: cfg, ConfigPath: filepath.Join(t.TempDir(), "config.json")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer inW.Close()
	defer outR.Close()
	defer outW.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, []string{"mcp", "stdio", "--addr", server.URL}, inR, outW, io.Discard) }()
	deadline, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	client := mcp.NewClient(&mcp.Implementation{Name: "cli-stdio-test", Version: "test"}, nil)
	session, err := client.Connect(deadline, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(deadline, &mcp.CallToolParams{Name: "torana_invoke", Arguments: map[string]any{"namespace": "torana", "operation": "system.status"}})
	if err != nil || result.IsError {
		t.Fatal("CLI stdio did not reach the real host operation dispatcher")
	}
	cancel()
	select {
	case <-done:
	case <-deadline.Done():
		t.Fatal("stdio CLI did not stop")
	}
}

func TestMCPTokenCLIOnlyExplicitSecretOutput(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("X-Torana-Local-Request") != "1" {
			t.Error("token request lost operator guard marker")
		}
		seen = append(seen, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"token":%q}`, token)
	}))
	defer server.Close()
	out, diag, err := invoke(server.URL, "", "mcp", "token")
	if err != nil || out != token+"\n" || diag != "" {
		t.Fatal("token command did not return only the token")
	}
	out, diag, err = invoke(server.URL, "", "mcp", "rotate", "--yes", "--json")
	var result struct{ Token string }
	if err != nil || diag != "" || json.Unmarshal([]byte(out), &result) != nil || result.Token != token {
		t.Fatal("JSON token output failed")
	}
	if len(seen) != 2 || seen[0] != "/_torana/api/v1/mcp/token" || seen[1] != "/_torana/api/v1/mcp/token/rotate" {
		t.Fatalf("wrong token route: %v", seen)
	}
}

func TestMCPMalformedTokenResponseIsNotPrinted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"token":"private-invalid-sentinel"}`)
	}))
	defer server.Close()
	out, diag, err := invoke(server.URL, "", "mcp", "token")
	if err == nil || out != "" || strings.Contains(err.Error()+diag, "private-invalid-sentinel") {
		t.Fatal("malformed token leaked into diagnostics/output")
	}
}

func TestMCPCLIRealOperatorRoundTrip(t *testing.T) {
	cfg := provider.DefaultConfig()
	cfg.Plugins.Dir = t.TempDir()
	cfg.MCP.Access = map[string]string{"torana.stats.get": "never"}
	s, err := proxy.New(proxy.Config{Providers: cfg, ConfigPath: filepath.Join(t.TempDir(), "config.json")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	out, _, err := invoke(server.URL, "", "mcp", "status")
	if err != nil || !strings.Contains(out, `"enabled": false`) {
		t.Fatal("initial status failed")
	}
	out, diag, err := invoke(server.URL, "", "mcp", "enable", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"enabled": true`) || strings.Contains(out, `"token"`) || diag != "" {
		t.Fatal("enable leaked a token or failed status")
	}
	current := s.GetConfig().Providers
	if !current.MCP.Enabled || current.MCP.Access["torana.stats.get"] != "never" || len(current.Providers) != len(cfg.Providers) {
		t.Fatal("enable changed unrelated settings")
	}
	first, diag, err := invoke(server.URL, "", "mcp", "token")
	if err != nil || diag != "" {
		t.Fatal("token retrieval failed")
	}
	second, _, err := invoke(server.URL, "", "mcp", "token")
	if err != nil || first != second {
		t.Fatal("repeated setup rotated credentials")
	}
	rotated, diag, err := invoke(server.URL, "", "mcp", "rotate", "--yes")
	if err != nil || rotated == first || diag != "" {
		t.Fatal("rotation failed")
	}
	out, _, err = invoke(server.URL, "", "mcp", "disable", "--yes")
	if err != nil || !strings.Contains(out, `"enabled": false`) {
		t.Fatal("disable failed")
	}
	retained, _, err := invoke(server.URL, "", "mcp", "token")
	if err != nil || retained != rotated {
		t.Fatal("disable did not retain the token")
	}
}
