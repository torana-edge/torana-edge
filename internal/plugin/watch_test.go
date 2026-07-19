package plugin

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/wasm"
)

// TestWatchPlugins_ReloadEditDeleteAndShutdown covers the hot-reload path
// (#142) that shipped several fixes in #140 with no test: a live configFn +
// runtimeFn (so config reloads and host callbacks survive), reload on edit,
// unload on delete, and watcher cancellation on ctx cancel.
//
// It uses two real compiled plugins as a swappable pair (schema_translator then
// auth), driven purely through the fsnotify path — no proxy needed.
func TestWatchPlugins_ReloadEditDeleteAndShutdown(t *testing.T) {
	schemaWASM := readWASM(t, "../../plugins/schema_translator/plugin.wasm")
	schemaJSON := readFile(t, "../../plugins/schema_translator/plugin.json")
	authWASM := readWASM(t, "../../plugins/auth/plugin.wasm")
	authJSON := readFile(t, "../../plugins/auth/plugin.json")

	dir := t.TempDir()
	pDir := filepath.Join(dir, "p")
	if err := os.MkdirAll(pDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(pDir, "plugin.json"), schemaJSON)
	writeFile(t, filepath.Join(pDir, "plugin.wasm"), schemaWASM)

	// Fresh-config / fresh-runtime guards: #140 shipped bugs precisely because
	// reloads captured a stale config and lost host callbacks. Count invocations
	// to prove WatchPlugins calls these factories anew on every reload.
	var configCalls, runtimeCalls atomic.Int32
	configFn := func() PluginConfig {
		configCalls.Add(1)
		return PluginConfig{Dir: dir, Order: []string{"schema_translator", "auth"}}
	}
	var rtMu sync.Mutex
	var runtimes []*wasm.Runtime
	runtimeFn := func() *wasm.Runtime {
		runtimeCalls.Add(1)
		rt := wasm.NewRuntime(context.Background())
		rtMu.Lock()
		runtimes = append(runtimes, rt)
		rtMu.Unlock()
		return rt
	}
	t.Cleanup(func() {
		rtMu.Lock()
		defer rtMu.Unlock()
		for _, rt := range runtimes {
			rt.Close()
		}
	})

	reloads := make(chan *PluginPipeline, 8)
	reloadFn := func(pp *PluginPipeline) { reloads <- pp }

	ctx, cancel := context.WithCancel(context.Background())
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		if err := WatchPlugins(ctx, dir, configFn, runtimeFn, reloadFn); err != nil {
			t.Errorf("WatchPlugins: %v", err)
		}
	}()

	// The debounce is 500ms; give reloads a generous window.
	awaitReload := func(what string) *PluginPipeline {
		t.Helper()
		select {
		case pp := <-reloads:
			return pp
		case <-time.After(6 * time.Second):
			t.Fatalf("no reload after %s", what)
			return nil
		}
	}

	// 1. Edit: turn the single schema_translator plugin dir into an auth plugin
	//    (different manifest name + wasm) → reload should now load "auth" only.
	writeFile(t, filepath.Join(pDir, "plugin.json"), authJSON)
	writeFile(t, filepath.Join(pDir, "plugin.wasm"), authWASM)
	pp := awaitReload("edit")
	if !hasLoaded(pp, "auth") {
		t.Errorf("after edit: expected 'auth' loaded, got %v", loadedNames(pp))
	}
	if hasLoaded(pp, "schema_translator") {
		t.Errorf("after edit: schema_translator should be gone, got %v", loadedNames(pp))
	}
	if configCalls.Load() < 1 || runtimeCalls.Load() < 1 {
		t.Errorf("reload didn't call fresh configFn/runtimeFn (config=%d runtime=%d) — #140 regression guard",
			configCalls.Load(), runtimeCalls.Load())
	}

	// 2. Delete: removing plugin.wasm must unload the plugin.
	if err := os.Remove(filepath.Join(pDir, "plugin.wasm")); err != nil {
		t.Fatal(err)
	}
	pp = awaitReload("delete")
	if pp.Len() != 0 {
		t.Errorf("after delete: expected 0 plugins loaded, got %d (%v)", pp.Len(), loadedNames(pp))
	}

	// 3. Shutdown: cancelling the context must exit the watcher goroutine.
	cancel()
	select {
	case <-watchDone:
	case <-time.After(3 * time.Second):
		t.Fatal("WatchPlugins goroutine did not exit after ctx cancel (watcher leak)")
	}
}

func hasLoaded(pp *PluginPipeline, name string) bool {
	for _, n := range loadedNames(pp) {
		if n == name {
			return true
		}
	}
	return false
}

func loadedNames(pp *PluginPipeline) []string {
	var out []string
	for _, lp := range pp.plugins {
		out = append(out, lp.manifest.Name)
	}
	return out
}

func readWASM(t *testing.T, path string) []byte {
	t.Helper()
	requireWASM(t, path)
	return readFile(t, path)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
