// Torana Edge – stateful AI FinOps reverse proxy.
//
// Entry point. Imports a seed config into Torana's managed store on first run,
// then loads the managed configuration, wires the proxy server, and blocks
// until the process receives a termination signal.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/torana-edge/torana-edge/internal/conversationcmd"
	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/plugincmd"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/proxy"

	// Register format adapters so their init() calls wire the registry.
	_ "github.com/torana-edge/torana-edge/internal/format/anthropic"
	_ "github.com/torana-edge/torana-edge/internal/format/bedrock"
	_ "github.com/torana-edge/torana-edge/internal/format/gemini"
	_ "github.com/torana-edge/torana-edge/internal/format/openai"
)

// version is injected for tagged builds by the Makefile. Untagged builds use
// "dev" or a commit SHA and intentionally skip product-version compatibility
// gates while continuing to enforce the plugin ABI and capability contract.
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "plugin" {
		if err := plugincmd.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
			log.Printf("plugin command: %v", err)
			os.Exit(2)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "conversations" {
		if err := conversationcmd.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
			log.Printf("conversations command: %v", err)
			os.Exit(2)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] != "serve" {
		log.Printf("unknown command %q (run without arguments or use: torana serve | torana plugin ... | torana conversations)", os.Args[1])
		os.Exit(2)
	}

	// --- configuration --------------------------------------------------
	seedPath := "config.json"
	if v := os.Getenv("TORANA_CONFIG"); v != "" {
		seedPath = v
	}

	storePath, err := provider.ManagedStorePath()
	if err != nil {
		log.Fatalf("Failed to resolve managed store path: %v", err)
	}

	// Fail closed. Downgrading to defaults here left PII blocking, compaction
	// and cost accounting silently off behind a single warning line — in a
	// process that usually runs in the background, where nobody reads it. A
	// proxy that silently stops enforcing the policy it was configured with is
	// worse than one that does not start.
	//
	// This does not affect a fresh install: a missing config is not an error,
	// it resolves to defaults and materializes the managed store. Reaching
	// here means the config exists and is broken, unreadable, or unwritable.
	provCfg, err := provider.ResolveConfig(seedPath, storePath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v\n\n"+
			"Torana will not start with a partial configuration, because the plugins you "+
			"configured — PII blocking, compaction, cost accounting — would be silently off.\n\n"+
			"Configuration is now validated on every load path, not only on control-plane "+
			"writes, so a rule that was previously enforced in one place may be reporting a "+
			"pre-existing problem for the first time.\n\n"+
			"Fix the reported field, or move %s aside to start from defaults.", err, storePath)
	}
	// A fallback with no credential of its own answers 401, which is not
	// retryable — so failover turns a recoverable 429 into a hard failure for
	// the caller. Say so now, not on the first 429.
	if unauth := provCfg.UnauthenticatedFallbacks(); len(unauth) > 0 {
		log.Printf("Warning: fallback provider(s) %v declare no API key and have not set "+
			"forward_caller_credential. Failover to them will send an unauthenticated request. "+
			"Set an api_key_env, or set forward_caller_credential:true if they are meant to "+
			"receive the caller's own credential (a second endpoint of the same vendor, or a "+
			"local model server).", unauth)
	}

	if differs, diffErr := provider.ManagedStoreShadowsSeed(seedPath, storePath); diffErr != nil {
		log.Printf("Warning: could not compare seed config %q with managed store %q: %v", seedPath, storePath, diffErr)
	} else if differs {
		log.Printf("Warning: managed config %q differs from and takes precedence over seed %q; edit the managed store through /_torana/ or remove it to re-import the seed", storePath, seedPath)
	}

	// Allow port override via env.
	if v := os.Getenv("TORANA_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			provCfg.Port = p
		}
	}

	cfg := proxy.Config{
		Port:            strconv.Itoa(provCfg.Port),
		HostVersion:     version,
		Providers:       provCfg,
		DefaultProvider: os.Getenv("TORANA_DEFAULT_PROVIDER"),
		ConfigPath:      storePath,
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

	bindHost := os.Getenv("TORANA_BIND")
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}
	if err := srv.Start(bindHost); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
	<-ctx.Done()
	log.Println("Shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	log.Println("Torana Edge stopped.")
}
