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

	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

type recordingPluginHTTPDispatcher struct {
	runID uint64
	endID uint64
}

func (dispatcher *recordingPluginHTTPDispatcher) RunOnHTTPRequest(_ context.Context, requestID uint64, _ string, _ *pb.HttpRequest) (*pb.HttpResponse, error) {
	dispatcher.runID = requestID
	// v2 dropped Handled: a non-nil response IS the plugin serving the
	// request, and declining is a nil response.
	return &pb.HttpResponse{Status: http.StatusOK, Body: []byte(`{}`)}, nil
}

func (dispatcher *recordingPluginHTTPDispatcher) EndRequest(requestID uint64) {
	dispatcher.endID = requestID
}

func TestPluginHTTPDispatchEndsRequestState(t *testing.T) {
	dispatcher := &recordingPluginHTTPDispatcher{}
	if _, err := dispatchPluginHTTPRequest(context.Background(), dispatcher, "test", &pb.HttpRequest{}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if dispatcher.runID == 0 || dispatcher.endID != dispatcher.runID {
		t.Fatalf("request cleanup mismatch: run=%d end=%d", dispatcher.runID, dispatcher.endID)
	}
}

func TestAgentControlPlaneDiscoveryAndJSONErrors(t *testing.T) {
	config := provider.DefaultConfig()
	config.Port = 8080
	server, err := New(Config{Port: "8080", Providers: config})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer server.Shutdown(context.Background())

	request := localControlPlaneRequest(http.MethodGet, "/_torana/api/v1/", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("discovery status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("discovery content type = %q", got)
	}
	var document agentAPIDocument
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode discovery: %v", err)
	}
	if document.APIVersion != "v1" || document.Security.Scope != "loopback-only" {
		t.Fatalf("unexpected discovery document: %+v", document)
	}
	foundPluginsList := false
	seenOperationIDs := map[string]bool{}
	for _, operation := range document.Operations {
		if seenOperationIDs[operation.ID] {
			t.Fatalf("duplicate discovery operation ID %q", operation.ID)
		}
		seenOperationIDs[operation.ID] = true
		if operation.ID == "torana.plugins.list" &&
			operation.Path == "/_torana/api/v1/plugins" &&
			operation.Risk == "read" {
			foundPluginsList = true
		}
	}
	if !foundPluginsList {
		t.Fatal("discovery missing torana.plugins.list")
	}
	for _, operation := range document.Operations {
		if operation.Risk != "read" || operation.Method != http.MethodGet || operation.Plugin != "" {
			continue
		}
		request = localControlPlaneRequest(http.MethodGet, operation.Path, nil)
		request.RemoteAddr = "127.0.0.1:12345"
		recorder = httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", operation.ID, recorder.Code, recorder.Body.String())
		}
		if err := plugin.ValidateAgentPayload(operation.OutputSchema, recorder.Body.Bytes()); err != nil {
			t.Fatalf("%s response violates advertised schema: %v\n%s", operation.ID, err, recorder.Body.String())
		}
	}

	request = localControlPlaneRequest(http.MethodPut, "/_torana/api/v1/config", strings.NewReader("{"))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("X-Torana-Local-Request", "1")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid config status = %d, want 400", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("error content type = %q, want application/json", got)
	}
	var envelope agentAPIErrorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if envelope.Error.Code != "invalid_request" || envelope.Error.Message == "" {
		t.Fatalf("unexpected error envelope: %+v", envelope)
	}

	for _, path := range []string{"/_torana/api/v1/stats", "/_torana/api/v1/stream"} {
		request = localControlPlaneRequest(http.MethodPost, path, nil)
		request.RemoteAddr = "127.0.0.1:12345"
		request.Header.Set("X-Torana-Local-Request", "1")
		recorder = httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want 405", path, recorder.Code)
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("%s did not return JSON error: %v", path, err)
		}
		if envelope.Error.Code != "method_not_allowed" {
			t.Fatalf("%s error = %+v", path, envelope)
		}
	}
}

func TestValidateAgentResponseHeaders(t *testing.T) {
	if err := validateAgentResponseHeaders(json.RawMessage(`{"Content-Type":["application/json; charset=utf-8"]}`)); err != nil {
		t.Fatalf("JSON content type rejected: %v", err)
	}
	for _, encoded := range []string{
		`{"Access-Control-Allow-Origin":["*"]}`,
		`{"Content-Length":["10"]}`,
		`{"Content-Type":["text/html"]}`,
	} {
		if err := validateAgentResponseHeaders(json.RawMessage(encoded)); err == nil {
			t.Fatalf("unsafe agent headers accepted: %s", encoded)
		}
	}
}

func TestPluginAgentOperationDispatch(t *testing.T) {
	if _, err := os.Stat(fixturesDir + "/test-http-server/plugin.wasm"); err != nil {
		t.Skip("test-http-server fixture is not built; run make testdata")
	}
	config := provider.DefaultConfig()
	config.Port = 8080
	config.Plugins = provider.PluginsConfig{
		Dir:             fixturesDir,
		Order:           []string{"test-http-server"},
		AllowUnapproved: true,
	}
	server, err := New(Config{Port: "8080", Providers: config})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer server.Shutdown(context.Background())
	handler := server.Handler()

	request := localControlPlaneRequest(http.MethodGet, "/_torana/api/v1/", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("discovery status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"id":"plugin:test-http-server:status"`) ||
		!strings.Contains(recorder.Body.String(), `"plugin_digest":"sha256:`) {
		t.Fatalf("discovery missing the fixture operation: %s", recorder.Body.String())
	}

	request = localControlPlaneRequest(http.MethodGet, "/_torana/api/v1/agent/plugins/test-http-server/status", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("agent status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("agent content type = %q", got)
	}
	var status map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode plugin status: %v", err)
	}
	if status["plugin"] != "test-http-server" || status["status"] != "ready" {
		t.Fatalf("unexpected plugin status: %v", status)
	}

	request = localControlPlaneRequest(http.MethodPost, "/_torana/api/v1/agent/plugins/test-http-server/status", strings.NewReader(`{}`))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("X-Torana-Local-Request", "1")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method status = %d, want 405", recorder.Code)
	}
	if recorder.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("Allow = %q, want GET", recorder.Header().Get("Allow"))
	}
}

func TestPluginListDistinguishesLoadedBundleFromChangedDiskBundle(t *testing.T) {
	sourceDir := fixturesDir + "/test-http-server"
	if _, err := os.Stat(filepath.Join(sourceDir, "plugin.wasm")); err != nil {
		t.Skip("test-http-server fixture is not built; run make testdata")
	}
	pluginsDir := t.TempDir()
	pluginDir := filepath.Join(pluginsDir, "test-http-server")
	if err := os.Mkdir(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"plugin.wasm", "plugin.json", "schema.json", "agent.json"} {
		body, err := os.ReadFile(filepath.Join(sourceDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pluginDir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	config := provider.DefaultConfig()
	config.Port = 8080
	config.Plugins = provider.PluginsConfig{
		Dir:             pluginsDir,
		Order:           []string{"test-http-server"},
		AllowUnapproved: true,
	}
	server, err := New(Config{Port: "8080", Providers: config})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer server.Shutdown(context.Background())

	agentPath := filepath.Join(pluginDir, "agent.json")
	agentBody, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(agentBody), "Test fixture agent operations.", "Changed on disk.", 1)
	if changed == string(agentBody) {
		t.Fatal("agent.json was not modified — the test would assert nothing")
	}
	if err := os.WriteFile(agentPath, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}

	request := localControlPlaneRequest(http.MethodGet, "/_torana/api/v1/plugins", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("plugins status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Plugins []struct {
			State        string                  `json:"state"`
			Digest       string                  `json:"digest"`
			LoadedDigest string                  `json:"loaded_digest"`
			Agent        *plugin.AgentDescriptor `json:"agent"`
			LoadedAgent  *plugin.AgentDescriptor `json:"loaded_agent"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Plugins) != 1 {
		t.Fatalf("plugins = %d, want 1", len(response.Plugins))
	}
	got := response.Plugins[0]
	if got.State != "stale" || got.Digest == got.LoadedDigest || got.Agent == nil || got.LoadedAgent == nil {
		t.Fatalf("disk/live state not represented: %+v", got)
	}
}

func TestPluginListReportsDuplicateBundleIdentity(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"first", "second"} {
		pluginDir := filepath.Join(root, directory)
		if err := os.Mkdir(pluginDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(`{"id":"community/example","name":"example"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pluginDir, "plugin.wasm"), []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	config := provider.DefaultConfig()
	server, err := New(Config{Port: "8080", Providers: config})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Shutdown(context.Background())
	config.Plugins.Dir = root
	server.SetProviders(config)

	request := localControlPlaneRequest(http.MethodGet, "/_torana/api/v1/plugins", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("plugins status = %d, want 500: %s", recorder.Code, recorder.Body.String())
	}
	var envelope agentAPIErrorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || envelope.Error.Code == "" {
		t.Fatalf("duplicate discovery did not return canonical JSON error: %v %s", err, recorder.Body.String())
	}
}
