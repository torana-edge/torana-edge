package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// Plugin configuration has two write paths and only one of them validated. The
// per-plugin POST checked the blob against the bundle's declared schema; the
// bulk PUT did not — and the dashboard's "Save & apply" button uses the bulk
// PUT. So the path operators actually click was the unchecked one, and a
// wrong-typed value reached the guest to misbehave silently.

// pluginConfigEnv stands up a real server over a temp plugin directory holding
// one bundle whose schema declares a boolean field.
type pluginConfigEnv struct {
	srv        *Server
	url        string
	configPath string
}

func newPluginConfigEnv(t *testing.T) *pluginConfigEnv {
	t.Helper()

	pluginsDir := t.TempDir()
	dir := filepath.Join(pluginsDir, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("plugin.json", `{"name":"settings","version":"0.1.0"}`)
	write("schema.json", `{"fields":[{"key":"enabled","type":"boolean","label":"Enabled"}]}`)
	if err := os.WriteFile(filepath.Join(dir, "plugin.wasm"),
		[]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}

	provCfg := provider.DefaultConfig()
	provCfg.Plugins = provider.PluginsConfig{Dir: pluginsDir, AllowUnapproved: true}

	configPath := filepath.Join(t.TempDir(), "config.json")
	srv, err := New(Config{
		Port:       "8080",
		Providers:  provCfg,
		ConfigPath: configPath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Shutdown(context.Background())
		ln.Close()
	})

	return &pluginConfigEnv{srv: srv, url: "http://" + ln.Addr().String(), configPath: configPath}
}

func (e *pluginConfigEnv) put(t *testing.T, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, e.url+"/_torana/api/plugins", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", e.url)

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("PUT /_torana/api/plugins: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *pluginConfigEnv) storedConfig(name string) string {
	return string(e.srv.GetConfig().Providers.Plugins.Config[name])
}

// TestBulkPluginConfigIsValidated is the regression test: a value of the wrong
// type for a declared field must be rejected here, exactly as the per-plugin
// endpoint rejects it.
func TestBulkPluginConfigIsValidated(t *testing.T) {
	env := newPluginConfigEnv(t)

	status, body := env.put(t, `{"config":{"settings":{"enabled":"yes-please"}}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("bulk PUT accepted a config the schema rejects: status %d, body %s", status, body)
	}
	if !strings.Contains(body, "settings") {
		t.Errorf("error does not name the offending plugin: %s", body)
	}
	if got := env.storedConfig("settings"); strings.Contains(got, "yes-please") {
		t.Errorf("rejected config was persisted anyway: %s", got)
	}
}

// TestBulkPluginConfigAcceptsValid guards the other direction — validation must
// not start rejecting configuration that conforms.
func TestBulkPluginConfigAcceptsValid(t *testing.T) {
	env := newPluginConfigEnv(t)

	status, body := env.put(t, `{"config":{"settings":{"enabled":true}}}`)
	if status != http.StatusOK {
		t.Fatalf("valid config rejected: status %d, body %s", status, body)
	}
	if got := env.storedConfig("settings"); !strings.Contains(got, "true") {
		t.Errorf("valid config was not persisted: %s", got)
	}
}

// TestBulkPluginConfigIgnoresUnknownPlugins: configuration for a plugin with no
// bundle on disk is deliberately retained rather than validated or dropped, so
// disabling a plugin does not lose its settings. The bulk path must match the
// per-plugin path in that.
func TestBulkPluginConfigIgnoresUnknownPlugins(t *testing.T) {
	env := newPluginConfigEnv(t)

	status, body := env.put(t, `{"config":{"not-on-disk":{"anything":1}}}`)
	if status != http.StatusOK {
		t.Fatalf("config for an absent plugin should be retained, not rejected: status %d, body %s", status, body)
	}
	if got := env.storedConfig("not-on-disk"); !strings.Contains(got, "anything") {
		t.Errorf("config for an absent plugin was dropped: %q", got)
	}
}

// TestSavePipelineDoesNotBlankConfigs pins the dashboard side of the same bug.
// savePipeline sent `configMap[name] || {}` for every plugin in the pipeline,
// and the server treats that map as a patch — so an enabled plugin whose config
// had not loaded had its stored settings silently overwritten with an empty
// object, just by clicking Save & apply.
func TestSavePipelineDoesNotBlankConfigs(t *testing.T) {
	spa, err := os.ReadFile("../controlplane/dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	// Scoped to savePipeline: other functions use `configMap[name] || {}`
	// legitimately, to render an empty form for an unconfigured plugin. Only
	// the body that builds the request is a problem.
	const start = "function savePipeline("
	i := strings.Index(string(spa), start)
	if i < 0 {
		t.Fatal("savePipeline not found in the dashboard")
	}
	body := string(spa)[i:]
	if j := strings.Index(body[len(start):], "\n    function "); j >= 0 {
		body = body[:len(start)+j]
	}

	if strings.Contains(body, "configMap[name] || {}") {
		t.Error("savePipeline still synthesises {} for a missing config, which overwrites stored settings")
	}
	if !strings.Contains(body, "configMap[name] !== undefined") {
		t.Error("savePipeline no longer guards on a present config; the blanking bug can recur")
	}
}

func TestDashboardRequiresExplicitPrivateFileApproval(t *testing.T) {
	spa, err := os.ReadFile("../controlplane/dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	source := string(spa)
	for _, required := range []string{
		".file-binding-enabled:checked",
		"max_bytes: Number(row.querySelector('.file-binding-bytes').value)",
		"retained_files: Number(row.querySelector('.file-binding-retained').value)",
		"files: files",
		"const enabled = Boolean(approvedFiles[file.path])",
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("dashboard private-file approval seam is missing %q", required)
		}
	}
	if strings.Contains(source, `class="file-binding-enabled" data-file="${escAttr(file.path)}" checked`) {
		t.Fatal("a new plugin's private file is approved by default")
	}
}

// TestUnchangedBadConfigDoesNotBlockPipelineEdits — the dashboard's Save &
// apply resubmits every enabled plugin's current config on every reorder. If
// validation ran over all of them, a single stored value predating a schema
// change would block enabling, disabling or reordering ANYTHING until it was
// fixed by hand, while the per-plugin endpoint would block only that plugin.
func TestUnchangedBadConfigDoesNotBlockPipelineEdits(t *testing.T) {
	env := newPluginConfigEnv(t)

	// Written through Save/Load, not injected in memory. provider.Save uses
	// json.MarshalIndent, which re-indents nested json.RawMessage — so what
	// comes back off disk is pretty-printed while the dashboard PUTs it
	// compact. Setting the value directly skipped that entirely, which is why
	// the first version of this test passed against a byte comparison that
	// could never match after a restart.
	provs := env.srv.GetConfig().Providers
	provs.Plugins.Config = map[string]json.RawMessage{"settings": json.RawMessage(`{"enabled":"legacy"}`)}
	if err := provider.Save(env.configPath, provs); err != nil {
		t.Fatal(err)
	}
	reloaded, err := provider.Load(env.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(reloaded.Plugins.Config["settings"]) == `{"enabled":"legacy"}` {
		t.Fatal("the fixture no longer exercises re-indentation; this test would be vacuous")
	}
	env.srv.SetProviders(reloaded)

	// Resubmitting the same value, compact as the dashboard sends it, must go
	// through. The dashboard does this on every save, including saves that only
	// reorder or toggle a different plugin.
	status, body := env.put(t, `{"config":{"settings":{"enabled":"legacy"}}}`)
	if status != http.StatusOK {
		t.Fatalf("an unchanged legacy value blocked an unrelated pipeline edit: status %d, body %s", status, body)
	}

	// Changing it to something still invalid must be rejected.
	status, _ = env.put(t, `{"config":{"settings":{"enabled":"also-bad"}}}`)
	if status != http.StatusBadRequest {
		t.Errorf("a NEW invalid value was accepted: status %d", status)
	}
}
