package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The installer activates complete directories by rename. There need not be a
// later write to any file inside them to rescue a missed directory event.
func TestWatchAtomicBundleInstallReplaceRemove(t *testing.T) {
	dir := t.TempDir()
	makeBundle := func(name string) string {
		t.Helper()
		stage := filepath.Join(dir, ".install-"+name)
		if err := os.Mkdir(stage, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(stage, "plugin.json"), readFile(t, fixturesDir+"/"+name+"/plugin.json"))
		writeFile(t, filepath.Join(stage, "plugin.wasm"), readWASM(t, fixturesDir+"/"+name+"/plugin.wasm"))
		return stage
	}
	stage := makeBundle("test-metrics")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	inventories := make(chan []PluginBundle, 16)
	if err := WatchPlugins(ctx, dir, func(ctx context.Context) error {
		bundles, err := DiscoverPlugins(dir)
		if err != nil {
			return err
		}
		select {
		case inventories <- bundles:
		case <-ctx.Done():
		}
		return nil
	}, func(err error) { t.Errorf("watcher: %v", err) }, func() { close(done) }); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-done })
	await := func(want string) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case bundles := <-inventories:
				if len(bundles) == 0 && want == "" {
					return
				}
				if len(bundles) == 1 && bundles[0].Manifest.Name == want {
					return
				}
			case <-deadline:
				t.Fatalf("directory operation did not publish inventory %q", want)
			}
		}
	}
	if bundles, err := DiscoverPlugins(dir); err != nil || len(bundles) != 0 {
		t.Fatalf("staged bundle visible: %v %v", bundles, err)
	}
	target := filepath.Join(dir, "active")
	if err := os.Rename(stage, target); err != nil {
		t.Fatal(err)
	}
	await("test-metrics")
	stage = makeBundle("test-mutator")
	if bundles, err := DiscoverPlugins(dir); err != nil || len(bundles) != 1 || bundles[0].Manifest.Name != "test-metrics" {
		t.Fatalf("stage changed inventory: %v %v", bundles, err)
	}
	if err := os.Rename(target, filepath.Join(dir, ".backup")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stage, target); err != nil {
		t.Fatal(err)
	}
	await("test-mutator")
	if err := os.Rename(target, filepath.Join(t.TempDir(), "removed")); err != nil {
		t.Fatal(err)
	}
	await("")
}
