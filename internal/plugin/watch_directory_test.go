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

func TestWatchIgnoresUnrelatedRootFileRemovals(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "README.md")
	if err := os.WriteFile(original, []byte("notes"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	reloads := make(chan struct{}, 10)
	if err := WatchPlugins(ctx, dir, func(context.Context) error { reloads <- struct{}{}; return nil }, nil, func() { close(done) }); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); <-done }()
	renamed := filepath.Join(dir, "notes.tmp")
	if err := os.Rename(original, renamed); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reloads:
		t.Fatal("unrelated file triggered reload")
	case <-time.After(time.Second):
	}
}

func TestWatchCancelledReloadDoesNotReportFailure(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	started := make(chan struct{})
	failures := make(chan error, 1)
	if err := WatchPlugins(ctx, dir, func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() }, func(err error) { failures <- err }, func() { close(done) }); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); <-done }()
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("reload not started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop")
	}
	select {
	case err := <-failures:
		t.Fatalf("shutdown marked reload failed: %v", err)
	default:
	}
}
