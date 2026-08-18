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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/torana-edge/torana-edge/internal/format/openai"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
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
	updateBody := `{"order":[],"config":{"schema_translator":{"strict":true}}}`
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

	if len(gotPlugins.Order) != 0 {
		t.Errorf("gotPlugins.Order = %v, want no enabled plugins", gotPlugins.Order)
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
	if len(savedCfg.Plugins.Order) != 0 {
		t.Errorf("persisted Plugins.Order = %v, want no enabled plugins", savedCfg.Plugins.Order)
	}
}

func localControlPlaneRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Host = "127.0.0.1"
	return req
}

type blockingMutationBody struct {
	reader  *strings.Reader
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (body *blockingMutationBody) Read(dst []byte) (int, error) {
	body.once.Do(func() {
		close(body.started)
		<-body.release
	})
	return body.reader.Read(dst)
}

type observedMutationBody struct {
	reader  *strings.Reader
	started chan struct{}
	once    sync.Once
}

func (body *observedMutationBody) Read(dst []byte) (int, error) {
	body.once.Do(func() { close(body.started) })
	return body.reader.Read(dst)
}

func authorizedControlPlaneMutation(method, target string, body io.Reader) *http.Request {
	req := localControlPlaneRequest(method, target, body)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Torana-Local-Request", "1")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestControlPlaneMutationsAreOneTransactionDomain(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := provider.DefaultConfig()
	cfg.Port = 8080
	srv, err := New(Config{Port: "8080", Providers: cfg, ConfigPath: configPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Shutdown(context.Background())

	incoming := cfg
	incoming.Limits.RPM = 17
	settingsJSON, err := json.Marshal(incoming)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	releaseFirst := make(chan struct{})
	defer func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
	}()
	firstStarted := make(chan struct{})
	firstBody := &blockingMutationBody{
		reader: strings.NewReader(string(settingsJSON)), started: firstStarted, release: releaseFirst,
	}
	firstDone := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		srv.Handler().ServeHTTP(recorder, authorizedControlPlaneMutation(http.MethodPut, "/_torana/api/config", firstBody))
		firstDone <- recorder.Code
	}()
	select {
	case <-firstStarted: // the settings transaction owns the lock and is inside body read
	case <-time.After(5 * time.Second):
		t.Fatal("settings mutation did not begin")
	}

	secondStarted := make(chan struct{})
	secondBody := &observedMutationBody{
		reader: strings.NewReader(`{"config":{"future":{"enabled":true}}}`), started: secondStarted,
	}
	secondDone := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		srv.Handler().ServeHTTP(recorder, authorizedControlPlaneMutation(http.MethodPut, "/_torana/api/plugins", secondBody))
		secondDone <- recorder.Code
	}()

	select {
	case <-secondStarted:
		close(releaseFirst)
		t.Fatal("plugin mutation read its candidate while the settings transaction was still open")
	case <-time.After(100 * time.Millisecond):
		// Expected: the second endpoint has not even consumed its candidate.
	}
	close(releaseFirst)
	if status := <-firstDone; status != http.StatusOK {
		t.Fatalf("settings status = %d, want 200", status)
	}
	select {
	case <-secondStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("plugin mutation did not resume after the settings transaction completed")
	}
	if status := <-secondDone; status != http.StatusOK {
		t.Fatalf("plugins status = %d, want 200", status)
	}
	got := srv.GetConfig().Providers
	if got.Limits.RPM != 17 || string(got.Plugins.Config["future"]) != `{"enabled":true}` {
		t.Fatalf("live config lost a serialized update: rpm=%d plugin=%s", got.Limits.RPM, got.Plugins.Config["future"])
	}
	persisted, err := provider.Load(configPath)
	if err != nil {
		t.Fatalf("load persisted config: %v", err)
	}
	if persisted.Limits.RPM != 17 || !sameJSONConfig(persisted.Plugins.Config["future"], json.RawMessage(`{"enabled":true}`)) {
		t.Fatalf("persisted config diverged: rpm=%d plugin=%s", persisted.Limits.RPM, persisted.Plugins.Config["future"])
	}
}

func TestControlPlaneMutationBodyLimitIsExact(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := provider.DefaultConfig()
	cfg.Port = 8080
	cfg.Plugins.Config = map[string]json.RawMessage{"known": json.RawMessage(`{}`)}
	srv, err := New(Config{Port: "8080", Providers: cfg, ConfigPath: configPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Shutdown(context.Background())

	jsonObjectOfSize := func(size int) string {
		const prefix, suffix = `{"padding":"`, `"}`
		if size < len(prefix)+len(suffix) {
			t.Fatalf("invalid fixture size %d", size)
		}
		return prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix
	}
	for _, row := range []struct {
		method string
		path   string
	}{
		{http.MethodPut, "/_torana/api/config"},
		{http.MethodPut, "/_torana/api/plugins"},
		{http.MethodPost, "/_torana/api/plugins/known/config"},
	} {
		recorder := httptest.NewRecorder()
		req := authorizedControlPlaneMutation(row.method, row.path, strings.NewReader(jsonObjectOfSize(maxBodySize+1)))
		srv.Handler().ServeHTTP(recorder, req)
		if recorder.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s %s status = %d, want 413", row.method, row.path, recorder.Code)
		}
	}

	// The boundary itself is permitted. This payload is a no-op plugins update;
	// it may be semantically rejected in the future, but never as too large.
	recorder := httptest.NewRecorder()
	req := authorizedControlPlaneMutation(http.MethodPut, "/_torana/api/plugins", strings.NewReader(jsonObjectOfSize(maxBodySize)))
	srv.Handler().ServeHTTP(recorder, req)
	if recorder.Code == http.StatusRequestEntityTooLarge {
		t.Fatal("exactly maxBodySize bytes were rejected")
	}
}

func TestControlPlaneMutationBodyReadFailuresStayBadRequests(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := provider.DefaultConfig()
	cfg.Port = 8080
	cfg.Plugins.Config = map[string]json.RawMessage{"known": json.RawMessage(`{}`)}
	srv, err := New(Config{Port: "8080", Providers: cfg, ConfigPath: configPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Shutdown(context.Background())

	for _, row := range []struct {
		method string
		path   string
	}{
		{http.MethodPut, "/_torana/api/config"},
		{http.MethodPut, "/_torana/api/plugins"},
		{http.MethodPost, "/_torana/api/plugins/known/config"},
	} {
		recorder := httptest.NewRecorder()
		req := authorizedControlPlaneMutation(row.method, row.path, failingRequestBody{err: io.ErrUnexpectedEOF})
		srv.Handler().ServeHTTP(recorder, req)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s %s status = %d, want 400", row.method, row.path, recorder.Code)
		}
	}
}

func TestControlPlanePluginsOrderingConstraintError(t *testing.T) {
	pluginsDir := t.TempDir()
	pGateDir := filepath.Join(pluginsDir, "gate")
	pRouterDir := filepath.Join(pluginsDir, "router")
	os.MkdirAll(pGateDir, 0755)
	os.MkdirAll(pRouterDir, 0755)

	// hooks must match what MinimalV2Module claims in supported_hooks: the host
	// now requires exact equality, so a manifest declaring nothing is a module
	// that can never be dispatched.
	gateManifest := `{"name":"gate","version":"0.1.0","abi_version":"v2","hooks":[{"name":"run_before_request"}],"permissions":[{"name":"env.host_call.torana_evaluate_compaction"}]}`
	routerManifest := `{"name":"router","version":"0.1.0","abi_version":"v2","hooks":[{"name":"run_before_request"}],"permissions":[{"name":"env.route_request"}]}`
	// A bare module header is no longer loadable: the v2 host reads
	// supported_hooks at load, so an empty module is correctly rejected as a
	// v1 guest. This test is about ordering constraints, not module validity,
	// so it needs a real minimal v2 guest.
	wasmBytes := wasm.MinimalV2Module(false)

	os.WriteFile(filepath.Join(pGateDir, "plugin.json"), []byte(gateManifest), 0644)
	os.WriteFile(filepath.Join(pGateDir, "plugin.wasm"), wasmBytes, 0644)
	os.WriteFile(filepath.Join(pRouterDir, "plugin.json"), []byte(routerManifest), 0644)
	os.WriteFile(filepath.Join(pRouterDir, "plugin.wasm"), wasmBytes, 0644)

	configPath := filepath.Join(t.TempDir(), "config.json")
	provCfg := provider.DefaultConfig()
	provCfg.Plugins = provider.PluginsConfig{
		Dir:             pluginsDir,
		Order:           []string{"router", "gate"},
		AllowUnapproved: true,
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
	originalPipeline := srv.pluginPipeline.Load()

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

	// The same economic constraint is enforced on the effective before-hook
	// order, and a rejected override is rolled back atomically.
	invalidHookBody := `{"hook_order":{"run_before_request":["gate","router"]}}`
	req, _ = http.NewRequest(http.MethodPost, url+"/_torana/api/plugins", bytes.NewBufferString(invalidHookBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", url)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST invalid hook_order: %v", err)
	}
	invalidHookResponse, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(invalidHookResponse), "hook_order.run_before_request") {
		t.Fatalf("invalid hook_order: status=%d body=%s", resp.StatusCode, invalidHookResponse)
	}
	if got := srv.GetConfig().Providers.Plugins.HookOrder; len(got) != 0 {
		t.Fatalf("rejected hook_order persisted: %v", got)
	}
	if got := srv.pluginPipeline.Load(); got != originalPipeline {
		t.Fatal("rejected hook_order replaced the live pipeline")
	}

	validHookBody := `{"hook_order":{"run_before_request":["router","gate"]}}`
	req, _ = http.NewRequest(http.MethodPost, url+"/_torana/api/plugins", bytes.NewBufferString(validHookBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", url)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST valid hook_order: %v", err)
	}
	validHookResponse, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid hook_order: status=%d body=%s", resp.StatusCode, validHookResponse)
	}
	if got := srv.GetConfig().Providers.Plugins.HookOrder["run_before_request"]; !slices.Equal(got, []string{"router", "gate"}) {
		t.Fatalf("hook_order was not applied: %v", got)
	}
	if got := srv.pluginPipeline.Load(); got == originalPipeline {
		t.Fatal("accepted hook_order did not publish a new immutable pipeline generation")
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
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Error("v1 response missing restrictive CSP")
		}
		if strings.Contains(csp, "sandbox") {
			t.Error("the trusted control-plane dashboard must not inherit the plugin document sandbox")
		}
	})

	t.Run("plugin documents have an opaque origin even when opened directly", func(t *testing.T) {
		handler := srv.controlPlanePluginGuard(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<script>fetch('/_torana/api/config')</script>`))
		})
		req := localControlPlaneRequest(http.MethodGet, "/_torana/plugin/hostile/", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "sandbox allow-scripts allow-forms") {
			t.Fatalf("plugin response CSP = %q, missing document sandbox", csp)
		}
		if strings.Contains(csp, "allow-same-origin") {
			t.Fatalf("plugin response CSP restores the control-plane origin: %q", csp)
		}
		if !strings.Contains(csp, "frame-ancestors 'self'") {
			t.Fatalf("plugin response CSP lost dashboard embedding: %q", csp)
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

		// A CSP-sandboxed plugin document has an opaque origin serialized by the
		// browser as "null". Even if it submits a form, that must not satisfy the
		// control-plane mutation guard.
		req = localControlPlaneRequest(http.MethodPost, "/_torana/api/v1/plugins", strings.NewReader(`{}`))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Origin", "null")
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("opaque-origin mutation status = %d, want 403", rec.Code)
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
			"/_torana/api/v1/",
			"/_torana/api/v1/agent/plugins/test/status",
			"/_torana/plugin/test",
			"/_torana/plugin/test?q=1",
			"/_torana/api/v1/agent/plugins/test/status?since=7",
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
