package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	updateBody := `{"order":["schema_translator","intent"],"config":{"schema_translator":{"strict":true}}}`
	provCfg.Plugins.Config = map[string]json.RawMessage{"disabled_plugin": json.RawMessage(`{"retain":true}`)}
	srv.SetProviders(provCfg)
	req, err := http.NewRequest(http.MethodPut, url+"/_torana/api/plugins", strings.NewReader(updateBody))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", url)

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
	if string(gotPlugins.Config["disabled_plugin"]) != `{"retain":true}` {
		t.Errorf("disabled plugin config was dropped: %s", gotPlugins.Config["disabled_plugin"])
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

func localControlPlaneRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Host = "127.0.0.1"
	return req
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
	req.Header.Set("Origin", url)

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

func TestControlPlaneGuard(t *testing.T) {
	provCfg := provider.DefaultConfig()
	provCfg.Port = 8080

	srv, err := New(Config{
		Port:      "8080",
		Providers: provCfg,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	handler := srv.Handler()

	t.Run("loopback allowed by default", func(t *testing.T) {
		req := localControlPlaneRequest(http.MethodGet, "/_torana/api/config", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("v1 returns hardened headers", func(t *testing.T) {
		req := localControlPlaneRequest(http.MethodGet, "/_torana/api/v1/config", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		for key, want := range map[string]string{
			"Cache-Control":          "no-store",
			"X-Content-Type-Options": "nosniff",
			"Referrer-Policy":        "no-referrer",
		} {
			if got := rec.Header().Get(key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Error("v1 response missing restrictive CSP")
		}
	})

	t.Run("rejects DNS-rebinding host and cross-origin mutation", func(t *testing.T) {
		req := localControlPlaneRequest(http.MethodGet, "/_torana/api/v1/config", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Host = "attacker.invalid"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("foreign Host status = %d, want 403", rec.Code)
		}

		req = localControlPlaneRequest(http.MethodPost, "/_torana/api/v1/plugins", strings.NewReader(`{}`))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Origin", "https://attacker.invalid")
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("cross-origin mutation status = %d, want 403", rec.Code)
		}
	})

	t.Run("non-loopback rejected when AllowRemote is false", func(t *testing.T) {
		req := localControlPlaneRequest(http.MethodGet, "/_torana/api/config", nil)
		req.RemoteAddr = "203.0.113.9:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "control plane is localhost-only") {
			t.Errorf("body = %q, want 'control plane is localhost-only'", rec.Body.String())
		}
	})

	t.Run("non-loopback remains rejected when deprecated remote settings are configured", func(t *testing.T) {
		remoteCfg := provCfg
		remoteCfg.ControlPlane = provider.ControlPlaneConfig{
			AllowRemote: true,
			Token:       "",
		}
		srvRemote, _ := New(Config{Port: "8080", Providers: remoteCfg})

		req := localControlPlaneRequest(http.MethodGet, "/_torana/api/config", nil)
		req.RemoteAddr = "203.0.113.9:12345"
		rec := httptest.NewRecorder()
		srvRemote.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("remote token does not enable the embedded control plane", func(t *testing.T) {
		tokCfg := provCfg
		tokCfg.ControlPlane = provider.ControlPlaneConfig{
			AllowRemote: true,
			Token:       "secret-token-123",
		}
		srvTok, _ := New(Config{Port: "8080", Providers: tokCfg})
		tokHandler := srvTok.Handler()

		// All remote callers are rejected: v1 is intentionally localhost-only.
		reqNoTok := localControlPlaneRequest(http.MethodGet, "/_torana/api/config", nil)
		reqNoTok.RemoteAddr = "203.0.113.9:12345"
		recNoTok := httptest.NewRecorder()
		tokHandler.ServeHTTP(recNoTok, reqNoTok)
		if recNoTok.Code != http.StatusForbidden {
			t.Errorf("no token status = %d, want 403", recNoTok.Code)
		}

		// A token is not an alternate remote auth mechanism.
		reqWrongTok := localControlPlaneRequest(http.MethodGet, "/_torana/api/config", nil)
		reqWrongTok.RemoteAddr = "203.0.113.9:12345"
		reqWrongTok.Header.Set("X-Torana-Token", "wrong-token")
		recWrongTok := httptest.NewRecorder()
		tokHandler.ServeHTTP(recWrongTok, reqWrongTok)
		if recWrongTok.Code != http.StatusForbidden {
			t.Errorf("wrong token status = %d, want 403", recWrongTok.Code)
		}

		// A correct legacy token also remains rejected remotely.
		reqHeader := localControlPlaneRequest(http.MethodGet, "/_torana/api/config", nil)
		reqHeader.RemoteAddr = "203.0.113.9:12345"
		reqHeader.Header.Set("X-Torana-Token", "secret-token-123")
		recHeader := httptest.NewRecorder()
		tokHandler.ServeHTTP(recHeader, reqHeader)
		if recHeader.Code != http.StatusForbidden {
			t.Errorf("X-Torana-Token status = %d, want 403", recHeader.Code)
		}

		// Nor does Authorization: Bearer.
		reqAuth := localControlPlaneRequest(http.MethodGet, "/_torana/api/config", nil)
		reqAuth.RemoteAddr = "203.0.113.9:12345"
		reqAuth.Header.Set("Authorization", "Bearer secret-token-123")
		recAuth := httptest.NewRecorder()
		tokHandler.ServeHTTP(recAuth, reqAuth)
		if recAuth.Code != http.StatusForbidden {
			t.Errorf("Authorization Bearer status = %d, want 403", recAuth.Code)
		}

		// Loopback caller with token configured -> 200 even without providing token
		reqLoopback := localControlPlaneRequest(http.MethodGet, "/_torana/api/config", nil)
		reqLoopback.RemoteAddr = "127.0.0.1:12345"
		recLoopback := httptest.NewRecorder()
		tokHandler.ServeHTTP(recLoopback, reqLoopback)
		if recLoopback.Code != http.StatusOK {
			t.Errorf("loopback with token configured status = %d, want 200", recLoopback.Code)
		}
	})

	t.Run("all /_torana endpoints are guarded", func(t *testing.T) {
		endpoints := []string{
			"/_torana/",
			"/_torana",
			"/_torana/api/feed",
			"/_torana/api/stream",
			"/_torana/api/config",
			"/_torana/api/plugins",
			"/_torana/plugin/test",
		}
		for _, ep := range endpoints {
			req := localControlPlaneRequest(http.MethodGet, ep, nil)
			req.RemoteAddr = "203.0.113.9:12345"
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("endpoint %s status = %d, want 403", ep, rec.Code)
			}
		}
	})

	t.Run("health is public but stats are control-plane data", func(t *testing.T) {
		for _, ep := range []string{"/health"} {
			req := localControlPlaneRequest(http.MethodGet, ep, nil)
			req.RemoteAddr = "203.0.113.9:12345"
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("endpoint %s status = %d, want 200", ep, rec.Code)
			}
		}
		req := localControlPlaneRequest(http.MethodGet, "/stats", nil)
		req.RemoteAddr = "203.0.113.9:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("remote stats status = %d, want 403", rec.Code)
		}
	})
}

func TestControlPlaneSecretRedaction(t *testing.T) {
	provCfg := provider.DefaultConfig()
	provCfg.Port = 8080
	provCfg.ControlPlane = provider.ControlPlaneConfig{
		AllowRemote: true,
		Token:       "super-secret-token-abcdef12345",
	}

	srv, err := New(Config{
		Port:      "8080",
		Providers: provCfg,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := localControlPlaneRequest(http.MethodGet, "/_torana/api/config", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	if strings.Contains(body, "super-secret-token-abcdef12345") {
		t.Errorf("GET /_torana/api/config leaked token in response body: %s", body)
	}
}

func TestControlPlanePortRebind(t *testing.T) {
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen 1: %v", err)
	}
	port1 := ln1.Addr().(*net.TCPAddr).Port
	ln1.Close()

	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen 2: %v", err)
	}
	port2 := ln2.Addr().(*net.TCPAddr).Port
	ln2.Close()

	provCfg := provider.DefaultConfig()
	provCfg.Port = port1

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	cfg := Config{
		Port:       strconv.Itoa(port1),
		Providers:  provCfg,
		ConfigPath: configPath,
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := srv.Start("127.0.0.1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Shutdown(context.Background())

	client := &http.Client{Timeout: 2 * time.Second}

	provCfg.Port = port2
	b, _ := json.Marshal(provCfg)
	req, err := http.NewRequest(http.MethodPut, fmt.Sprintf("http://127.0.0.1:%d/_torana/api/config", port1), bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", port1))

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT config: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
	}

	resp2, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port2))
	if err != nil {
		t.Fatalf("GET port2: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("port2 status = %d, want 200", resp2.StatusCode)
	}
}
