package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	credentialapi "github.com/torana-edge/torana-edge/credential"
	"github.com/torana-edge/torana-edge/internal/provider"
)

// countingProvider is a credential provider that does nothing. What matters is
// the factory that makes it.
type countingProvider struct{}

func (countingProvider) Resolve(context.Context, string) ([]byte, error) {
	return []byte("secret"), nil
}

// registerAlternatingFactory registers a credential provider type whose
// construction succeeds exactly once and fails every time after.
//
// This is the shape a real custom provider can have: construction is allowed
// to be stateful, to acquire a resource, or to fail transiently. Registration
// is process-wide and cannot be undone, so each call uses a fresh type name.
func registerAlternatingFactory(t *testing.T) (typeName string, builds *atomic.Int32) {
	t.Helper()
	builds = &atomic.Int32{}
	typeName = fmt.Sprintf("alternating-%s", strings.ReplaceAll(t.Name(), "/", "-"))
	err := credentialapi.RegisterProvider(typeName, func(json.RawMessage) (credentialapi.Provider, error) {
		if builds.Add(1) > 1 {
			return nil, fmt.Errorf("this provider can only be constructed once")
		}
		return countingProvider{}, nil
	})
	if err != nil {
		t.Fatalf("registering the test credential provider: %v", err)
	}
	return typeName, builds
}

// An accepted update must build its credential registry exactly once, and
// publish the object it built.
//
// Checking that a registry CAN be built and then building a second one at
// publication time is not a check — the object that was validated is not the
// object that goes live. With a provider whose construction can fail the second
// time, the double build is directly observable: preparation succeeds, the
// pipeline goes live, and the second construction then fails with the file
// already written and the new pipeline already executing.
func TestAnAcceptedPluginUpdateBuildsTheCredentialRegistryOnce(t *testing.T) {
	sourceType, builds := registerAlternatingFactory(t)

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
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

	// Install the source after startup, so the server's own construction is
	// not the one build this provider allows.
	live := srv.GetConfig().Providers
	live.Credentials.Sources = map[string]provider.CredentialSource{"vault": {Type: sourceType}}
	srv.configMu.Lock()
	srv.config.Providers = live
	srv.configMu.Unlock()
	if got := builds.Load(); got != 0 {
		t.Fatalf("the provider was built %d times before the update; the test cannot "+
			"distinguish one build from two", got)
	}

	url := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest(http.MethodPut, url+"/_torana/api/plugins",
		strings.NewReader(`{"order":[],"config":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", url)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT plugins: %v", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a valid update was refused with %d: %s\n"+
			"If this is 500, the registry was built a second time at publication and that "+
			"build failed — with the config already on disk and the new pipeline already live.",
			resp.StatusCode, payload)
	}
	if got := builds.Load(); got != 1 {
		t.Errorf("the credential registry was built %d times for one update, want 1. "+
			"The object that was validated is not the object that was published.", got)
	}

	// And the published registry must be the working one.
	if _, err := srv.liveCredentialRegistry().Resolve(context.Background(), "vault", "k"); err != nil {
		t.Errorf("the live credential registry does not resolve: %v", err)
	}
}

// When preparation fails, nothing may move: not the file, not the live config,
// not the running pipeline.
func TestAFailedCredentialPreparationLeavesEverythingAlone(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
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

	beforeBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	beforePipeline := livePipelineOrder(srv)
	if beforePipeline == "(none)" || beforePipeline == "" {
		t.Fatalf("no plugin is loaded, so the pipeline check compares nothing against "+
			"nothing: %q", beforePipeline)
	}

	live := srv.GetConfig().Providers
	live.Credentials.Sources = map[string]provider.CredentialSource{
		"broken": {Type: "no-such-provider-type"},
	}
	srv.configMu.Lock()
	srv.config.Providers = live
	srv.configMu.Unlock()
	beforeConfig := srv.GetConfig().Providers
	beforeRegistry := srv.liveCredentialRegistry()

	url := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest(http.MethodPut, url+"/_torana/api/plugins",
		strings.NewReader(`{"order":[],"config":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", url)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT plugins: %v", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, payload)
	}
	if after, err := os.ReadFile(configPath); err != nil {
		t.Fatal(err)
	} else if string(after) != string(beforeBytes) {
		t.Errorf("the rejected update was left on disk.\n before: %s\n  after: %s", beforeBytes, after)
	}
	if got := livePipelineOrder(srv); got != beforePipeline {
		t.Errorf("the rejected plugin order is live: pipeline %q, want %q", got, beforePipeline)
	}
	afterConfig := srv.GetConfig().Providers
	if len(afterConfig.Plugins.Order) != len(beforeConfig.Plugins.Order) {
		t.Errorf("the live config changed: plugin order %v, want %v",
			afterConfig.Plugins.Order, beforeConfig.Plugins.Order)
	}
	if srv.liveCredentialRegistry() != beforeRegistry {
		t.Error("the live credential registry was replaced by a rejected update")
	}
}
