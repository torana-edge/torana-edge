package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
)

// A rejected plugin update must leave NOTHING changed: not the file, not the
// live config, not the running pipeline.
//
// The first version of this fix returned the credential-registry rejection from
// SetProviders and rolled the config file back — but both handlers publish the
// new pipeline (rebuildPipelineReportingSkips) BEFORE calling SetProviders, so
// a rejection restored the file and left the process executing the rejected
// plugin order. The disk/live divergence was not removed, only moved to
// another component.
//
// This exercises the HANDLER, which is the only level at which that ordering is
// visible; a test calling SetProviders directly cannot see it.
func TestRejectedPluginUpdateChangesNothing(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	// A REAL plugin must be live, or the pipeline assertion below guards
	// nothing: an empty pipeline is trivially equal to an empty pipeline.
	requireWASM(t, fixturesDir+"/test-observer/plugin.wasm")

	provCfg := provider.DefaultConfig()
	provCfg.Port = 8080
	provCfg.Plugins = provider.PluginsConfig{
		Dir:             fixturesDir,
		Order:           []string{"test-observer"},
		AllowUnapproved: true,
	}
	if err := provider.Save(configPath, provCfg); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{Port: "8080", Providers: provCfg, ConfigPath: configPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	url := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 10 * time.Second}

	// Capture everything the update must not disturb.
	beforeBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeConfig := srv.GetConfig().Providers
	beforePipeline := livePipelineOrder(srv)
	if beforePipeline == "(none)" || beforePipeline == "" {
		t.Fatalf("no plugin is loaded, so the pipeline check below would compare nothing "+
			"against nothing: %q", beforePipeline)
	}

	// A credential source with no type cannot build a registry. Carried on the
	// same request that changes the plugin order, so the handler has a real
	// pipeline to publish before it would otherwise notice.
	bad := beforeConfig
	bad.Credentials.Sources = map[string]provider.CredentialSource{"broken": {Type: ""}}
	srv.configMu.Lock()
	srv.config.Providers = bad
	srv.configMu.Unlock()

	// A DIFFERENT order, so publishing it is observable in the live pipeline.
	body := `{"order":[],"config":{}}`
	req, _ := http.NewRequest(http.MethodPut, url+"/_torana/api/plugins", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", url)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT plugins: %v", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("a plugin update carrying an unbuildable credential registry was accepted: %s", payload)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — the caller's configuration is what is wrong: %s",
			resp.StatusCode, payload)
	}

	if after, err := os.ReadFile(configPath); err != nil {
		t.Fatal(err)
	} else if string(after) != string(beforeBytes) {
		t.Errorf("the rejected update was left on disk.\n before: %s\n  after: %s", beforeBytes, after)
	}
	if got := livePipelineOrder(srv); got != beforePipeline {
		t.Errorf("the rejected plugin order is live: pipeline %q, want the original %q.\n"+
			"The process is executing a configuration the caller was told was refused.",
			got, beforePipeline)
	}
}

// livePipelineOrder names the plugins the process is actually executing, so a
// test can tell whether a rejected update went live. It reads the same atomic
// the request path reads.
func livePipelineOrder(s *Server) string {
	pp, ok := s.pluginPipeline.Load().(*plugin.PluginPipeline)
	if !ok || pp == nil {
		return "(none)"
	}
	names := make([]string, 0, pp.Len())
	for _, st := range pp.LoadedPlugins() {
		names = append(names, st.Name)
	}
	return strings.Join(names, ",")
}
