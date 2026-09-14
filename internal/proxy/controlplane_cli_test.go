package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/controlcmd"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/testfixture"
)

func runControlCLI(t *testing.T, addr, input string, args ...string) (string, error) {
	t.Helper()
	var out, diag bytes.Buffer
	args = append(args, "--addr", addr)
	err := controlcmd.Run(context.Background(), args, strings.NewReader(input), &out, &diag)
	return out.String(), err
}

func TestCLIApproveEnableInvokeDisableAndRevokeRealGuest(t *testing.T) {
	const name = "test-http-server"
	testfixture.Require(t, filepath.Join(fixturesDir, name, "plugin.wasm"))
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join(fixturesDir, name))); err != nil {
		t.Fatal(err)
	}
	bundle, err := plugin.ValidateBundleDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := provider.DefaultConfig()
	cfg.Plugins = provider.PluginsConfig{Dir: filepath.Dir(dir)}
	// Discover only this fixture; the bundle must live in a named subdirectory.
	root := t.TempDir()
	if err := os.Rename(dir, filepath.Join(root, name)); err != nil {
		t.Fatal(err)
	}
	cfg.Plugins.Dir = root
	srv, err := New(Config{Port: "8080", Providers: cfg, ConfigPath: filepath.Join(t.TempDir(), "config.json")})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	approval := provider.PluginApproval{Digest: bundle.Digest, FailureMode: "block", Permissions: []string{}}
	for _, permission := range bundle.Manifest.Permissions {
		approval.Permissions = append(approval.Permissions, permission.Name)
	}
	body, _ := json.Marshal(approval)
	if _, err := runControlCLI(t, httpServer.URL, string(body), "plugin", "approve", name, "--file", "-", "--yes"); err != nil {
		t.Fatal(err)
	}
	if len(srv.GetConfig().Providers.Plugins.Order) != 0 {
		t.Fatal("approval enabled guest")
	}
	if _, err := runControlCLI(t, httpServer.URL, "", "plugin", "enable", name, "--yes"); err != nil {
		t.Fatal(err)
	}
	status, err := runControlCLI(t, httpServer.URL, "", "plugin", "inspect", name)
	if err != nil || !strings.Contains(status, `"loaded": true`) {
		t.Fatalf("not loaded: %s %v", status, err)
	}
	response, err := runControlCLI(t, httpServer.URL, "", "agent", "call", "plugin:test-http-server:status")
	if err != nil || !strings.Contains(response, `"status": "ready"`) {
		t.Fatalf("real guest response=%s err=%v", response, err)
	}
	req := authorizedControlPlaneMutation(http.MethodGet, "/_torana/api/v1/agent/plugins/test-http-server/status", nil)
	req.Header.Set("X-Torana-Plugin-Digest", "sha256:stale")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 412 || !strings.Contains(rec.Body.String(), "stale_plugin_digest") {
		t.Fatalf("stale operation status=%d body=%s", rec.Code, rec.Body)
	}
	for _, action := range []string{"disable", "enable", "revoke"} {
		if _, err := runControlCLI(t, httpServer.URL, "", "plugin", action, name, "--yes"); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	if len(srv.GetConfig().Providers.Plugins.Approvals) != 0 || len(srv.GetConfig().Providers.Plugins.Order) != 0 {
		t.Fatal("revoke left approval or enabled order")
	}
}

func TestCLILiveConfigSnapshotValidationPersistenceAndStaleWrite(t *testing.T) {
	e := newPluginConfigEnv(t)
	snapshot, err := runControlCLI(t, e.url, "", "config", "get")
	if err != nil {
		t.Fatal(err)
	}
	var edit struct {
		Revision string         `json:"revision"`
		Config   map[string]any `json:"config"`
	}
	if err := json.Unmarshal([]byte(snapshot), &edit); err != nil {
		t.Fatal(err)
	}
	if edit.Config["plugins"] != nil {
		t.Fatal("settings snapshot contains ignored plugin fields")
	}
	edit.Config["limits"] = map[string]int{"rpm": 77, "concurrency": 0}
	body, _ := json.Marshal(edit)
	if _, err := runControlCLI(t, e.url, string(body), "config", "apply", "--file", "-", "--yes"); err != nil {
		t.Fatal(err)
	}
	if got := e.srv.GetConfig().Providers.Limits.RPM; got != 77 {
		t.Fatalf("live RPM=%d", got)
	}
	stored, err := provider.Load(e.configPath)
	if err != nil || stored.Limits.RPM != 77 {
		t.Fatalf("persisted RPM=%d err=%v", stored.Limits.RPM, err)
	}
	if stored.Plugins.Dir != e.srv.GetConfig().Providers.Plugins.Dir {
		t.Fatal("settings replaced plugin configuration")
	}
	if _, err := runControlCLI(t, e.url, snapshot, "config", "apply", "--file", "-", "--yes"); err == nil || !strings.Contains(err.Error(), "fresh snapshot") {
		t.Fatalf("stale write error=%v", err)
	}
	if e.srv.GetConfig().Providers.Limits.RPM != 77 {
		t.Fatal("stale write changed live settings")
	}
	edit.Revision = strings.Repeat("bad", 22)
	body, _ = json.Marshal(edit)
	if _, err := runControlCLI(t, e.url, string(body), "config", "apply", "--file", "-", "--yes"); err == nil {
		t.Fatal("invalid revision accepted")
	}
}

func TestCLIPluginConfigUsesHostSchemaAndCrossEndpointRevision(t *testing.T) {
	e := newPluginConfigEnv(t)
	settings, err := runControlCLI(t, e.url, "", "config", "get")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := runControlCLI(t, e.url, "", "plugin", "config", "get", "settings")
	if err != nil {
		t.Fatal(err)
	}
	var edit struct {
		Revision string         `json:"revision"`
		Config   map[string]any `json:"config"`
	}
	json.Unmarshal([]byte(snapshot), &edit)
	edit.Config["enabled"] = "not-a-boolean"
	body, _ := json.Marshal(edit)
	if _, err := runControlCLI(t, e.url, string(body), "plugin", "config", "apply", "settings", "--file", "-", "--yes"); err == nil {
		t.Fatal("host schema was bypassed")
	}
	edit.Config["enabled"] = true
	body, _ = json.Marshal(edit)
	if _, err := runControlCLI(t, e.url, string(body), "plugin", "config", "apply", "settings", "--file", "-", "--yes"); err != nil {
		t.Fatal(err)
	}
	if e.storedConfig("settings") != `{"enabled":true}` {
		t.Fatalf("config=%s", e.storedConfig("settings"))
	}
	if _, err := runControlCLI(t, e.url, settings, "config", "apply", "--file", "-", "--yes"); err == nil {
		t.Fatal("settings snapshot survived intervening plugin edit")
	}
	if _, err := runControlCLI(t, e.url, snapshot, "plugin", "config", "apply", "settings", "--file", "-", "--yes"); err == nil {
		t.Fatal("stale plugin settings applied")
	}
}

func TestRevisionCoversSecretsWithoutExposingTheirUnkeyedHash(t *testing.T) {
	e := newPluginConfigEnv(t)
	cfg := e.srv.GetConfig().Providers
	first := e.srv.configRevision(cfg)
	cfg.Cache.Redis.PasswordEnc = "not-a-real-secret"
	if first == e.srv.configRevision(cfg) {
		t.Fatal("secret mutation did not change revision")
	}
	other := newPluginConfigEnv(t)
	if other.srv.configRevision(cfg) == e.srv.configRevision(cfg) {
		t.Fatal("revision is not process-specific")
	}
}

func TestPipelineRevisionIsReturnedAndCheckedBeforePersistence(t *testing.T) {
	e := newPluginConfigEnv(t)
	req := localControlPlaneRequest(http.MethodGet, "/_torana/api/v1/plugins", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("ETag") == "" {
		t.Fatalf("GET status=%d headers=%v", rec.Code, rec.Header())
	}
	before, _ := os.ReadFile(e.configPath)
	req = authorizedControlPlaneMutation("PUT", "/_torana/api/v1/plugins", strings.NewReader(`{"order":["missing"]}`))
	req.Header.Set("If-Match", `"outdated"`)
	rec = httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 412 || !strings.Contains(rec.Body.String(), `"code":"stale_revision"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	after, _ := os.ReadFile(e.configPath)
	if !bytes.Equal(before, after) {
		t.Fatal("stale mutation persisted")
	}
}
