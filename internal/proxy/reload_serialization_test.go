package proxy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/cache"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
)

// Start a filesystem reload in the admin transaction's publication gap. It
// must wait for the config commit, then build the disabled configuration.
func TestFilesystemReloadWaitsForAdministrativeConfigCommit(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-observer/plugin.wasm")
	pcfg := provider.PluginsConfig{Dir: fixturesDir, Order: []string{"test-observer"}, AllowUnapproved: true}
	s := &Server{config: Config{Providers: provider.Config{Plugins: pcfg}}}
	store := cache.NewLocalCache(time.Minute)
	s.setCache(store)
	defer store.Close()
	if err := s.RebuildPipeline(pcfg); err != nil {
		t.Fatal(err)
	}
	defer func() { s.pluginPipeline.Load().(*plugin.PluginPipeline).DrainAndClose() }()
	s.controlPlaneMutationMu.Lock()
	var unlock sync.Once
	defer unlock.Do(s.controlPlaneMutationMu.Unlock)
	disabled := pcfg
	disabled.Order = nil
	if err := s.RebuildPipeline(disabled); err != nil {
		t.Fatal(err)
	}
	// The old config is still visible here, as in the administrative handler.
	done := make(chan error, 1)
	go func() { done <- s.reloadPluginsFromFilesystem(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("reload escaped the config transaction: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	s.configMu.Lock()
	s.config.Providers.Plugins = disabled
	s.configMu.Unlock()
	unlock.Do(s.controlPlaneMutationMu.Unlock)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("reload did not finish after config publication")
	}
	if got := s.pluginPipeline.Load().(*plugin.PluginPipeline).Len(); got != 0 {
		t.Fatalf("filesystem reload restored %d disabled plugins", got)
	}
}

type observedCacheClose struct {
	cache.Store
	closed chan struct{}
	once   sync.Once
}

func (c *observedCacheClose) Close() { c.once.Do(func() { c.Store.Close(); close(c.closed) }) }

// Cache replacement must also wait for request-pinned generations displaced
// before the currently active pipeline. Waiting for only the last one is unsafe.
func TestCacheRetirementWaitsForEveryPipelineGeneration(t *testing.T) {
	s := &Server{config: Config{Providers: provider.Config{Plugins: provider.PluginsConfig{Dir: t.TempDir()}}}}
	oldStore := &observedCacheClose{Store: cache.NewLocalCache(time.Minute), closed: make(chan struct{})}
	s.setCache(oldStore)
	defer oldStore.Close()
	if err := s.RebuildPipeline(s.config.Providers.Plugins); err != nil {
		t.Fatal(err)
	}
	first := s.pluginPipeline.Load().(*plugin.PluginPipeline)
	if !first.TryAcquire() {
		t.Fatal("could not pin initial pipeline")
	}
	var release sync.Once
	defer release.Do(first.Release)
	if err := s.reloadPluginsFromFilesystem(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconfigureCache(cache.Config{}); err != nil {
		t.Fatal(err)
	}
	defer s.currentCache().Close()
	defer s.pluginPipeline.Load().(*plugin.PluginPipeline).DrainAndClose()
	select {
	case <-oldStore.closed:
		t.Fatal("old cache closed while an earlier generation was pinned")
	case <-time.After(100 * time.Millisecond):
	}
	release.Do(first.Release)
	select {
	case <-oldStore.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("cache did not close after the final user drained")
	}
}
