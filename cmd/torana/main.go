// Torana Edge – stateful AI FinOps reverse proxy.
//
// Entry point. Imports a seed config into Torana's managed store on first run,
// then loads the managed configuration, wires the proxy server, and blocks
// until the process receives a termination signal.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/torana-edge/torana-edge/internal/controlcmd"
	"github.com/torana-edge/torana-edge/internal/conversationcmd"
	"github.com/torana-edge/torana-edge/internal/credentialcmd"
	"github.com/torana-edge/torana-edge/internal/harnesscmd"
	"github.com/torana-edge/torana-edge/internal/instance"
	"github.com/torana-edge/torana-edge/internal/lifecyclecmd"
	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/plugincmd"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/proxy"

	// The outbound field-policy registry is validated once at startup; a
	// broken table would make every verifier decision garbage.
	"github.com/torana-edge/torana-plugin-sdk/outboundpolicy"

	// Register format adapters so their init() calls wire the registry.
	_ "github.com/torana-edge/torana-edge/internal/format/anthropic"
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
// It is the whole discoverable surface of the binary: without the environment
// table, the only way to learn that TORANA_BIND or TORANA_DATA_DIR exists is to
// read main(). TestUsageDocumentsEveryEnvironmentVariable keeps it complete.
func usage(w io.Writer) {
	fmt.Fprint(w, `torana — a local-first LLM reverse proxy for AI coding agents

Usage:
  torana [serve]                 run the proxy (default)
  torana --debug [serve]         run with safe per-request debug logs
  torana start                   run the proxy in the background
  torana status                  inspect the running instance
  torana stop --yes              stop it gracefully
  torana plugin <command>        author, build and install plugins
  torana credential <command>    configure named credentials
  torana conversations <command> inspect recorded conversations
  torana config <command>        inspect or apply live settings
  torana pipeline <command>      configure plugin order and approvals
  torana stats                   inspect aggregate request statistics
  torana feed [--follow]         inspect recent or live request events
  torana agent <command>         discover and call plugin agent operations
  torana mcp <command>           enable MCP, inspect it, or manage its token
  torana changes <command>       list or undo confirmed plugin changes
  torana harness <command>       connect MCP to Claude Code or Codex
  torana version                 print the version
  torana help                    print this message

Environment:
  TORANA_CONFIG            seed config path (default: config.json)
  TORANA_DATA_DIR          directory holding the managed store, which lives at
                           $TORANA_DATA_DIR/config.json
                           (default: os.UserConfigDir()/torana)
  TORANA_PORT              listen port, overriding the config (serve --port wins)
  TORANA_BIND              bind address (default: 127.0.0.1; serve --bind wins)
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
  TORANA_DEBUG_UPSTREAM_ERRORS
                           set to 1 with debug logging to log up to 8 KiB of
                           bridged upstream error bodies; may expose prompts
                           or credentials. Disabled by default.
  TORANA_CI_CACHE          directory for wazero's compiled-module cache; set it
                           and each plugin compiles once per machine rather
                           than once per start
  OTEL_EXPORTER_OTLP_ENDPOINT
                           OTLP gRPC collector. With neither endpoint set,
                           OTel export is off. An https:// endpoint uses TLS,
                           http:// is plaintext, and a bare host:port uses TLS
                           unless the matching _INSECURE variable is true.
                           Torana reads these only to decide whether to export;
                           the OTLP exporter itself applies the OpenTelemetry
                           precedence rules, so OTEL_EXPORTER_OTLP_HEADERS and
                           the other standard variables work as documented.
  OTEL_EXPORTER_OTLP_METRICS_ENDPOINT
                           as above, and takes precedence over it
  OTEL_EXPORTER_OTLP_INSECURE
                           set to true to allow plaintext to a scheme-less
                           OTLP endpoint
  OTEL_EXPORTER_OTLP_METRICS_INSECURE
                           as above, and takes precedence over it

serve accepts --port and --bind, which override the environment variables below
and the configured port. torana start forwards both to the instance it launches.

The control plane is at http://127.0.0.1:<port>/_torana/ and is reachable from
loopback only. Plugins never load until you approve their digest in the UI or CLI.
`)
	controlcmd.Usage(w)
	lifecyclecmd.Usage(w)
}

// controlPlaneHost names a host an operator can actually paste into a browser,
// and reports whether the control plane is reachable on this listener at all.
// The host is returned UNBRACKETED; net.JoinHostPort adds brackets for IPv6,
// and returning "[::1]" here produced "http://[[::1]]:8143/".
//
// The control plane requires a loopback REMOTE ADDRESS and a loopback Host, so
// the only URL worth printing is a loopback one — and that URL only works if
// the listening socket is bound somewhere loopback traffic can reach it.
//
//   - a loopback bind serves it at that address;
//   - a wildcard bind covers loopback, so name the loopback address of the
//     matching family: 127.0.0.1 for 0.0.0.0, ::1 for ::. An IPv6 socket may
//     be v6-only, in which case 127.0.0.1 is not listening;
//   - a SPECIFIC non-loopback bind (TORANA_BIND=192.168.1.10) serves the
//     control plane nowhere. Connecting to the bind address is refused by the
//     guard as a non-loopback source; connecting to 127.0.0.1 is refused by
//     the kernel, because nothing is listening there. Verified: 403 and
//     connection-refused respectively.
//
// The last case is why this returns a bool. Printing the bind address there
// advertises a URL that cannot work.
func controlPlaneHost(bindHost string) (string, bool) {
	host := strings.TrimSuffix(strings.TrimPrefix(bindHost, "["), "]")
	if host == "" {
		// net.Listen treats an empty host as every interface.
		return "127.0.0.1", true
	}
	if strings.EqualFold(host, "localhost") {
		return host, true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A name this binary cannot classify. Saying nothing is better than
		// naming a URL that may not answer.
		return "", false
	}
	switch {
	case ip.IsUnspecified():
		if ip.To4() != nil {
			return "127.0.0.1", true
		}
		return "::1", true
	case ip.IsLoopback():
		return host, true
	default:
		return "", false
	}
}

// serveOptions are the listener settings `serve` accepts on the command line.
// Zero values mean "not given", which is what lets the environment and then the
// configuration supply them in turn.
type serveOptions struct {
	port int
	bind string
}

// parseServeFlags reads `serve`'s flags. Before these existed the only way to
// move off 8080 was an environment variable or a config edit, and `serve`
// silently ignored anything typed after it — so `torana serve --port 9090`
// listened on 8080 and reported success.
//
// --bind accepts the same values as TORANA_BIND, deliberately including
// non-loopback ones: this is the documented way to serve the data plane wider,
// and rejecting here while accepting the environment would just move the
// footgun rather than remove it. The control plane keeps its own loopback
// requirement regardless of how the socket is bound.
func parseServeFlags(args []string, stderr io.Writer) (serveOptions, error) {
	var opts serveOptions
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }
	port := fs.String("port", "", "listen port, overriding TORANA_PORT and the configured port")
	fs.StringVar(&opts.bind, "bind", "", "bind address (default: 127.0.0.1)")
	if err := fs.Parse(args); err != nil {
		return serveOptions{}, err
	}
	if fs.NArg() != 0 {
		return serveOptions{}, fmt.Errorf("serve takes no positional arguments, got %q", fs.Arg(0))
	}
	if *port != "" {
		p, err := strconv.Atoi(*port)
		if err != nil {
			return serveOptions{}, fmt.Errorf("--port=%q is not a number", *port)
		}
		if p < 1 || p > 65535 {
			return serveOptions{}, fmt.Errorf("--port=%d is outside the valid port range 1-65535", p)
		}
		opts.port = p
	}
	return opts, nil
}

// parsePortOverride reads TORANA_PORT. A malformed or out-of-range value is an
// error, not something to ignore: this binary fails closed on every other
// configuration mistake, and silently dropping TORANA_PORT=808O means
// listening on the config's port while the operator believes the override took
// — found much later, from the wrong end.
//
// Separated from main so the decision can be tested. A first-hour failure mode
// living inside an untestable function is one cleanup away from returning.
func parsePortOverride(v string) (int, error) {
	p, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("TORANA_PORT=%q is not a number", v)
	}
	if p < 1 || p > 65535 {
		return 0, fmt.Errorf("TORANA_PORT=%d is outside the valid port range 1-65535", p)
	}
	return p, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--debug" {
		_ = os.Setenv("TORANA_LOG_LEVEL", "debug")
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
	}
	if len(os.Args) > 1 && os.Args[1] == "harness" {
		if err := harnesscmd.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}
	if lifecyclecmd.Handles(os.Args[1:]) {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err := lifecyclecmd.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
		stop()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(2)
		}
		return
	}
	if controlcmd.Handles(os.Args[1:]) {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err := controlcmd.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
		stop()
		if err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(2)
		}
		return
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
	var serveFlags serveOptions
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println(version)
			return
		case "help", "--help", "-h":
			usage(os.Stdout)
			return
		case "serve":
			var err error
			serveFlags, err = parseServeFlags(os.Args[2:], os.Stderr)
			if err != nil {
				if errors.Is(err, flag.ErrHelp) {
					return
				}
				fmt.Fprintf(os.Stderr, "%v\n\n", err)
				usage(os.Stderr)
				os.Exit(2)
			}
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
	owner, err := instance.Acquire(filepath.Join(filepath.Dir(storePath), "instance.lock"))
	if err != nil {
		log.Fatalf("Cannot start Torana: %v", err)
	}
	defer func() { _ = owner.Close() }()

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

	// Precedence: --port, then TORANA_PORT, then the configured port. A flag is
	// the most local statement of intent, so it wins over an inherited
	// environment — otherwise an exported TORANA_PORT in the shell would
	// silently beat the port the operator just typed.
	if serveFlags.port != 0 {
		provCfg.Port = serveFlags.port
	} else if v := os.Getenv("TORANA_PORT"); v != "" {
		p, err := parsePortOverride(v)
		if err != nil {
			log.Fatalf("%v", err)
		}
		provCfg.Port = p
	}

	cfg := proxy.Config{
		Port:               strconv.Itoa(provCfg.Port),
		HostVersion:        version,
		Providers:          provCfg,
		DefaultProvider:    os.Getenv("TORANA_DEFAULT_PROVIDER"),
		ConfigPath:         storePath,
		InstanceRecordPath: filepath.Join(filepath.Dir(storePath), "instance.json"),
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

	bindHost := serveFlags.bind
	if bindHost == "" {
		bindHost = os.Getenv("TORANA_BIND")
	}
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}
	if err := srv.Start(bindHost); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
	// Say where it is. Starting Torana printed one line about the plugin
	// pipeline and nothing else, so the quickstart's "keep that terminal
	// open" asked an operator to trust a process that had told them neither
	// that it was listening nor where — and the control plane, which is where
	// plugins are approved, was discoverable only by reading the README.
	log.Printf("Torana Edge %s listening on http://%s", version,
		net.JoinHostPort(bindHost, strconv.Itoa(provCfg.Port)))
	if cpHost, reachable := controlPlaneHost(bindHost); reachable {
		log.Printf("Control plane: http://%s/_torana/ (loopback only)",
			net.JoinHostPort(cpHost, strconv.Itoa(provCfg.Port)))
	} else {
		log.Printf("Control plane: UNREACHABLE with TORANA_BIND=%s. It requires a "+
			"loopback source address, and nothing is listening on loopback. "+
			"Bind a wildcard address (0.0.0.0 or ::) to serve both, or 127.0.0.1 "+
			"for loopback only. Plugins cannot be approved until then.", bindHost)
	}
	select {
	case <-ctx.Done():
	case <-srv.StopRequested():
	}
	log.Println("Shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A shutdown error is almost always the 5s deadline expiring with requests
	// still in flight. Say so: the alternative is a clean-looking exit that
	// silently cut live streams.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Shutdown did not complete cleanly: %v", err)
	}
	log.Println("Torana Edge stopped.")
}
