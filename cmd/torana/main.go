// Torana Edge – stateful AI FinOps reverse proxy.
//
// Entry point. Imports a seed config into Torana's managed store on first run,
// then loads the managed configuration, wires the proxy server, and blocks
// until the process receives a termination signal.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/torana-edge/torana-edge/internal/conversationcmd"
	"github.com/torana-edge/torana-edge/internal/credentialcmd"
	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/plugincmd"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/proxy"

	// The outbound field-policy registry is validated once at startup; a
	// broken table would make every verifier decision garbage.
	"github.com/torana-edge/torana-plugin-sdk/outboundpolicy"

	// Register format adapters so their init() calls wire the registry.
	_ "github.com/torana-edge/torana-edge/internal/format/anthropic"
	_ "github.com/torana-edge/torana-edge/internal/format/bedrock"
	_ "github.com/torana-edge/torana-edge/internal/format/gemini"
	_ "github.com/torana-edge/torana-edge/internal/format/openai"
)

// version is injected by release builds. `go install module@version` does not
// run the Makefile, so init falls back to the module version embedded by the Go
// toolchain. Keep the variable's initializer a string constant: Go's -X linker
// flag only owns variables with that shape. Local builds retain "dev";
// pseudo-versions remain identifiable as development builds and do not claim
// the compatibility semantics of a tag.
var version = "dev"

func init() {
	version = detectVersion(version)
}

func detectVersion(linked string) string {
	if linked != "dev" {
		return linked
	}
	info, ok := debug.ReadBuildInfo()
	return versionFromBuildInfo(linked, info, ok)
}

func versionFromBuildInfo(fallback string, info *debug.BuildInfo, ok bool) string {
	if fallback != "dev" {
		return fallback
	}
	if !ok || info == nil || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return fallback
	}
	return info.Main.Version
}

// validateOutboundPolicy verifies the outbound enforcement inventory of the
// pinned SDK once, as the serve path requires before it will start. Extracted
// from main() so it is testable without starting the server.
func validateOutboundPolicy() error {
	return outboundpolicy.Validate()
}

// usage documents the commands and every environment variable Torana reads.
//
// Both `torana --help` and `torana version` used to exit as unknown commands,
// which is the first thing anyone types. The environment table matters as much,
// because until now the only way to learn that TORANA_BIND or TORANA_DATA_DIR
// existed was to read main().
func usage(w io.Writer) {
	fmt.Fprint(w, `torana — a local-first LLM reverse proxy for AI coding agents

Usage:
  torana [serve]                 run the proxy (default)
  torana --debug [serve]         run with safe per-request debug logs
  torana plugin <command>        author, build and install plugins
  torana credential <command>    configure named credentials
  torana conversations <command> inspect recorded conversations
  torana version                 print the version
  torana help                    print this message

Environment:
  TORANA_CONFIG            seed config path (default: config.json)
  TORANA_DATA_DIR          directory holding the managed store, which lives at
                           $TORANA_DATA_DIR/config.json
                           (default: os.UserConfigDir()/torana)
  TORANA_PORT              listen port, overriding the config
  TORANA_BIND              bind address (default: 127.0.0.1)
                           Binding wider exposes the DATA PLANE only: the
                           control plane separately requires a loopback source
                           address and refuses remote requests. But a reverse
                           proxy forwarding to 127.0.0.1 makes every request
                           look local, which opens the control plane to anyone
                           who can reach it — so that proxy must block or
                           authenticate /_torana/* itself.
  TORANA_DEFAULT_PROVIDER  provider for requests that match no /provider/ prefix
  TORANA_PLUGINS_DIR       plugin directory for the plugin subcommands
  TORANA_LOG_LEVEL         set to debug for safe request lifecycle logs

The control plane is at http://127.0.0.1:<port>/_torana/ and is reachable from
loopback only. Plugins never load until you approve their digest there.
`)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--debug" {
		_ = os.Setenv("TORANA_LOG_LEVEL", "debug")
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
	}
	if len(os.Args) > 1 && os.Args[1] == "plugin" {
		if err := plugincmd.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
			log.Printf("plugin command: %v", err)
			os.Exit(2)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "credential" {
		if err := credentialcmd.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
			log.Printf("credential command: %v", err)
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
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println(version)
			return
		case "help", "--help", "-h":
			usage(os.Stdout)
			return
		case "serve":
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
			usage(os.Stderr)
			os.Exit(2)
		}
	}

	// The SDK self-validates its outbound field-policy registry (every proto
	// field has exactly one policy, delegate targets complete, signature
	// bindings valid). A broken policy table means every verifier decision is
	// garbage, so refuse to start rather than answer with wrong verdicts.
	// Validate exactly once at startup, per the SDK contract.
	if err := validateOutboundPolicy(); err != nil {
		log.Fatalf("Failed to validate outbound policy registry: %v", err)
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
	// A fallback explicitly configured with no authentication will usually
	// answer 401. Say so now, not on the first 429.
	if unauth := provCfg.UnauthenticatedFallbacks(); len(unauth) > 0 {
		log.Printf("Warning: fallback provider(s) %v use auth.mode=none; failover sends an unauthenticated request", unauth)
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
