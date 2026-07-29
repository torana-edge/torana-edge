package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/wasm"
)

// A fresh install has no ./plugins directory: nothing creates it but
// `torana plugin install`, and a git clone does not carry one. The watcher used
// to fail on the missing directory and take the whole proxy down with it, so
// Torana would not start until the operator had installed a plugin they may not
// have wanted.
func TestWatchPluginsCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugins")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("precondition: %s should not exist", dir)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt := wasm.NewRuntime(ctx)
	t.Cleanup(func() { _ = rt.Close() })

	done := make(chan struct{})
	err := WatchPlugins(ctx, dir,
		func() PluginConfig { return PluginConfig{Dir: dir} },
		func() *wasm.Runtime { return rt },
		func(*PluginPipeline) {},
		func(error) {},
		func() { close(done) },
	)
	if err != nil {
		t.Fatalf("WatchPlugins on a missing directory: %v", err)
	}

	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("watcher did not create %s: %v", dir, err)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not shut down")
	}
}
