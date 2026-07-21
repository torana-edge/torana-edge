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

	_ "github.com/torana-edge/torana-edge/internal/format/openai"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestControlPlaneConfigAPI(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	provCfg := provider.DefaultConfig()
	provCfg.Port = 8080

	cfg := Config{
		Port:       "8080",
		Providers:  provCfg,
		ConfigPath: configPath,
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	client := &http.Client{Timeout: 5 * time.Second}
	url := "http://" + ln.Addr().String()

	// GET /_torana/api/config
	resp, err := client.Get(url + "/_torana/api/config")
	if err != nil {
		t.Fatalf("GET /_torana/api/config: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /_torana/api/config status = %d, want 200", resp.StatusCode)
	}
	var gotCfg provider.Config
	if err := json.NewDecoder(resp.Body).Decode(&gotCfg); err != nil {
		t.Fatalf("decode GET config: %v", err)
	}
	if gotCfg.Port != 8080 {
		t.Errorf("gotCfg.Port = %d, want 8080", gotCfg.Port)
	}

	// PUT /_torana/api/plugins
	updateBody := `{"order":["schema_translator","intent"]}`
	req, err := http.NewRequest(http.MethodPut, url+"/_torana/api/plugins", strings.NewReader(updateBody))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp2, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT /_torana/api/plugins: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp2.Body)
		t.Fatalf("PUT /_torana/api/plugins status = %d, want 200: %s", resp2.StatusCode, string(b))
	}

	var gotPlugins provider.PluginsConfig
	if err := json.NewDecoder(resp2.Body).Decode(&gotPlugins); err != nil {
		t.Fatalf("decode PUT plugins response: %v", err)
	}

	if len(gotPlugins.Order) != 2 || gotPlugins.Order[0] != "schema_translator" || gotPlugins.Order[1] != "intent" {
		t.Errorf("gotPlugins.Order = %v, want [schema_translator intent]", gotPlugins.Order)
	}

	// Verify persistence on disk
	savedCfg, err := provider.Load(configPath)
	if err != nil {
		t.Fatalf("Load persisted config: %v", err)
	}
	if !savedCfg.Managed {
		t.Errorf("persisted config should be managed")
	}
	if len(savedCfg.Plugins.Order) != 2 || savedCfg.Plugins.Order[0] != "schema_translator" {
		t.Errorf("persisted Plugins.Order = %v, want [schema_translator intent]", savedCfg.Plugins.Order)
	}
}

func TestControlPlanePluginsOrderingConstraintError(t *testing.T) {
	pluginsDir := t.TempDir()
	pGateDir := filepath.Join(pluginsDir, "gate")
	pRouterDir := filepath.Join(pluginsDir, "router")
	os.MkdirAll(pGateDir, 0755)
	os.MkdirAll(pRouterDir, 0755)

	gateManifest := `{"name":"gate","version":"0.1.0","permissions":[{"name":"env.host_call.torana_evaluate_compaction"}]}`
	routerManifest := `{"name":"router","version":"0.1.0","permissions":[{"name":"env.route_request"}]}`
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

	os.WriteFile(filepath.Join(pGateDir, "plugin.json"), []byte(gateManifest), 0644)
	os.WriteFile(filepath.Join(pGateDir, "plugin.wasm"), wasmBytes, 0644)
	os.WriteFile(filepath.Join(pRouterDir, "plugin.json"), []byte(routerManifest), 0644)
	os.WriteFile(filepath.Join(pRouterDir, "plugin.wasm"), wasmBytes, 0644)

	configPath := filepath.Join(t.TempDir(), "config.json")
	provCfg := provider.DefaultConfig()
	provCfg.Plugins = provider.PluginsConfig{
		Dir:   pluginsDir,
		Order: []string{"router", "gate"},
	}

	cfg := Config{
		Port:       "8080",
		Providers:  provCfg,
		ConfigPath: configPath,
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	client := &http.Client{Timeout: 5 * time.Second}
	url := "http://" + ln.Addr().String()

	// Try setting an invalid order: gate before router -> must return HTTP 400
	invalidBody := `{"order":["gate","router"]}`
	req, _ := http.NewRequest(http.MethodPost, url+"/_torana/api/plugins", bytes.NewBufferString(invalidBody))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /_torana/api/plugins: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected status 400 for ordering constraint violation, got %d: %s", resp.StatusCode, string(b))
	}

	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "ordering constraint violation") {
		t.Errorf("expected error message to contain 'ordering constraint violation', got: %s", string(b))
	}

	// Verify in-memory config was reverted to valid order
	currentPlugins := srv.GetConfig().Providers.Plugins
	if len(currentPlugins.Order) != 2 || currentPlugins.Order[0] != "router" {
		t.Errorf("in-memory config was not reverted, current order: %v", currentPlugins.Order)
	}
}
