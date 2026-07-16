// Torana Edge – stateful AI FinOps reverse proxy.
//
// Entry point.  Loads provider configuration from config.json (falls back
// to built-in defaults), wires the proxy server, and blocks until the
// process receives a termination signal.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/proxy"

	// Register format adapters so their init() calls wire the registry.
	_ "github.com/torana-edge/torana-edge/internal/format/anthropic"
	_ "github.com/torana-edge/torana-edge/internal/format/bedrock"
	_ "github.com/torana-edge/torana-edge/internal/format/openai"
	_ "github.com/torana-edge/torana-edge/internal/format/vertex"
)

func main() {
	// --- configuration --------------------------------------------------
	configPath := "config.json"
	if v := os.Getenv("TORANA_CONFIG"); v != "" {
		configPath = v
	}

	provCfg, err := provider.Load(configPath)
	if err != nil {
		log.Printf("Warning: %v — using defaults", err)
		provCfg = provider.DefaultConfig()
	}

	// Allow port override via env.
	if v := os.Getenv("TORANA_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			provCfg.Port = p
		}
	}

	cfg := proxy.Config{
		Port:            strconv.Itoa(provCfg.Port),
		Providers:       provCfg,
		DefaultProvider: os.Getenv("TORANA_DEFAULT_PROVIDER"),
	}

	// Initialize OTel BEFORE the server so New can bridge its StatsTracker to
	// the meter (RegisterStatsObservables is a no-op if OTel is disabled).
	if otelShutdown, err := metrics.InitOTel(context.Background()); err == nil {
		//nolint:errcheck
		defer otelShutdown(context.Background())
	} else {
		log.Printf("Failed to init OTel: %v", err)
	}

	// --- server ---------------------------------------------------------
	srv, err := proxy.New(cfg)
	if err != nil {
		log.Fatalf("Failed to create proxy server: %v", err)
	}

	// Graceful shutdown on Ctrl+C / SIGTERM (Docker/K8s).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("panic in shutdown goroutine: %v", r)
			}
		}()
		<-ctx.Done()
		log.Println("Shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	// Start config hot-reload watcher.
	stopWatch := provider.WatchConfig(configPath, 5*time.Second, func(newCfg provider.Config) {
		srv.SetProviders(newCfg)
	})
	defer stopWatch()

	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
	log.Println("Torana Edge stopped.")
}
