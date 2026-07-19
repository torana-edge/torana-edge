package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/wasm"
)

// TestReloadPipeline_ReflectsDiskState deterministically covers the reload
// machinery that #140 fixed but left untested — WITHOUT depending on fsnotify
// event delivery (which is unreliable on CI filesystems). reloadPipeline is
// exactly what WatchPlugins' debounced timer calls on every change, so driving
// it directly with fresh config + a fresh runtime proves:
//   - a reload reflects the CURRENT on-disk config (the "live config" fix), and
//   - removing a plugin unloads it (the fsnotify.Remove → unload fix).
func TestReloadPipeline_ReflectsDiskState(t *testing.T) {
	schemaWASM := readWASM(t, "../../plugins/schema_translator/plugin.wasm")
	schemaJSON := readFile(t, "../../plugins/schema_translator/plugin.json")
	authWASM := readWASM(t, "../../plugins/auth/plugin.wasm")
	authJSON := readFile(t, "../../plugins/auth/plugin.json")

	dir := t.TempDir()
	pDir := filepath.Join(dir, "p")
	if err := os.MkdirAll(pDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := PluginConfig{Dir: dir, Order: []string{"schema_translator", "auth"}}

	// reload runs reloadPipeline with a FRESH runtime each time — the runtime
	// factory fix — so host callbacks survive reloads rather than being lost.
	reload := func() *PluginPipeline {
		t.Helper()
		rt := wasm.NewRuntime(context.Background())
		t.Cleanup(func() { rt.Close() })
		pp, err := reloadPipeline(rt, cfg)
		if err != nil {
			t.Fatalf("reloadPipeline: %v", err)
		}
		return pp
	}

	// Initial: schema_translator on disk → loaded.
	writeFile(t, filepath.Join(pDir, "plugin.json"), schemaJSON)
	writeFile(t, filepath.Join(pDir, "plugin.wasm"), schemaWASM)
	if pp := reload(); !hasLoaded(pp, "schema_translator") {
		t.Fatalf("initial: schema_translator not loaded, got %v", loadedNames(pp))
	}

	// Edit: replace with auth → next reload reflects the new on-disk config.
	writeFile(t, filepath.Join(pDir, "plugin.json"), authJSON)
	writeFile(t, filepath.Join(pDir, "plugin.wasm"), authWASM)
	pp := reload()
	if !hasLoaded(pp, "auth") {
		t.Errorf("after edit: expected 'auth', got %v", loadedNames(pp))
	}
	if hasLoaded(pp, "schema_translator") {
		t.Errorf("after edit: schema_translator should be gone, got %v", loadedNames(pp))
	}

	// Delete: remove the plugin files → next reload unloads it (0 plugins).
	os.Remove(filepath.Join(pDir, "plugin.wasm"))
	os.Remove(filepath.Join(pDir, "plugin.json"))
	if pp := reload(); pp.Len() != 0 {
		t.Errorf("after delete: expected 0 plugins, got %d (%v)", pp.Len(), loadedNames(pp))
	}
}

// TestWatchPlugins_FiresReloadAndExitsOnCancel covers the fsnotify wiring of
// WatchPlugins: a file change triggers a reload (via a live configFn + a fresh
// runtimeFn), and cancelling the context exits the watcher goroutine.
//
// fsnotify delivery is flaky on CI filesystems (events coalesce/drop), so the
// reload half re-writes the plugin on an interval until a reload lands — a plain
// re-write always emits fresh events, so this needs no delete/create trickery.
// The load-bearing reload semantics (fresh config, unload) are pinned
// deterministically in TestReloadPipeline_ReflectsDiskState above; this test's
// job is just the wiring.
func TestWatchPlugins_FiresReloadAndExitsOnCancel(t *testing.T) {
	authWASM := readWASM(t, "../../plugins/auth/plugin.wasm")
	authJSON := readFile(t, "../../plugins/auth/plugin.json")
	schemaWASM := readWASM(t, "../../plugins/schema_translator/plugin.wasm")
	schemaJSON := readFile(t, "../../plugins/schema_translator/plugin.json")

	dir := t.TempDir()
	pDir := filepath.Join(dir, "p")
	if err := os.MkdirAll(pDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(pDir, "plugin.json"), schemaJSON)
	writeFile(t, filepath.Join(pDir, "plugin.wasm"), schemaWASM)

	var configCalls, runtimeCalls int
	configFn := func() PluginConfig {
		configCalls++
		return PluginConfig{Dir: dir, Order: []string{"schema_translator", "auth"}}
	}
	var runtimes []*wasm.Runtime
	runtimeFn := func() *wasm.Runtime {
		runtimeCalls++
		rt := wasm.NewRuntime(context.Background())
		runtimes = append(runtimes, rt)
		return rt
	}
	t.Cleanup(func() {
		for _, rt := range runtimes {
			rt.Close()
		}
	})

	reloads := make(chan *PluginPipeline, 16)
	ctx, cancel := context.WithCancel(context.Background())
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		if err := WatchPlugins(ctx, dir, configFn, runtimeFn, func(pp *PluginPipeline) { reloads <- pp }, nil); err != nil {
			t.Errorf("WatchPlugins: %v", err)
		}
	}()

	// Edit → wait for a reload, re-writing on an interval to survive dropped
	// fsnotify events. A plain re-write always emits a Write event.
	deadline := time.After(60 * time.Second)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	touch := func() {
		writeFile(t, filepath.Join(pDir, "plugin.json"), authJSON)
		writeFile(t, filepath.Join(pDir, "plugin.wasm"), authWASM)
	}
	touch()
	var got *PluginPipeline
wait:
	for {
		select {
		case pp := <-reloads:
			got = pp
			break wait
		case <-tick.C:
			touch()
		case <-deadline:
			t.Fatal("WatchPlugins never fired a reload after an edit (fsnotify wiring broken)")
		}
	}
	if !hasLoaded(got, "auth") {
		t.Errorf("reload didn't reflect the edit: got %v", loadedNames(got))
	}
	if configCalls < 1 || runtimeCalls < 1 {
		t.Errorf("reload didn't use fresh configFn/runtimeFn (config=%d runtime=%d)", configCalls, runtimeCalls)
	}

	// Cancel → the watcher goroutine must exit (deterministic).
	cancel()
	select {
	case <-watchDone:
	case <-time.After(10 * time.Second):
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
