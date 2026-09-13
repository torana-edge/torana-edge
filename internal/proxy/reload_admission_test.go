package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

// A request may observe a generation immediately before a reload drains it.
// Exercise the rejected acquisition deterministically: a draining pipeline
// must not turn a configured proxy into a proxy without policy enforcement.
func TestDrainingPipelineRefusesRequestAdmission(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()
	srv, err := New(Config{Providers: testProviderConfig(upstream.URL, "test", "openai")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	rt := wasm.NewRuntime(context.Background())
	pp, err := plugin.NewPipeline(rt, plugin.PluginConfig{Dir: t.TempDir()})
	if err != nil {
		_ = rt.Close()
		t.Fatal(err)
	}
	srv.pluginPipeline.Store(pp)
	pp.DrainAndClose()

	request := httptest.NewRequest(http.MethodPost, "/provider/test/v1/chat/completions",
		strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"private"}]}`))
	response := httptest.NewRecorder()
	logs := captureLogs(t)
	srv.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", response.Code, response.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("request bypassed the draining pipeline and reached upstream")
	}
	if response.Header().Get("Retry-After") != "1" {
		t.Fatal("refusal omitted retry backoff")
	}
	if !strings.Contains(logs.String(), "plugin request admission refused") {
		t.Fatal("refusal was not logged")
	}
}

// Model the exact swap-before-drain interleaving without timing a goroutine:
// admission starts with the stale pointer while the atomic slot holds its replacement.
func TestRequestAdmissionAcquiresReplacementPipeline(t *testing.T) {
	newPipeline := func() *plugin.PluginPipeline {
		rt := wasm.NewRuntime(context.Background())
		pp, err := plugin.NewPipeline(rt, plugin.PluginConfig{Dir: t.TempDir()})
		if err != nil {
			_ = rt.Close()
			t.Fatal(err)
		}
		t.Cleanup(pp.DrainAndClose)
		return pp
	}
	old, replacement := newPipeline(), newPipeline()
	srv := &Server{}
	srv.pluginPipeline.Store(replacement)
	old.DrainAndClose()
	got := srv.acquireRequestPipeline(old)
	if got == nil {
		t.Fatal("servable replacement was refused")
	}
	defer got.Release()
	if got != replacement {
		t.Fatal("wrong generation acquired")
	}
}
