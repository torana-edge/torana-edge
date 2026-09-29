// Package lifecyclecmd controls a local Torana instance without trusting PID
// files or signaling unrelated processes. Shutdown is an identity-bound API call.
package lifecyclecmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/torana-edge/torana-edge/internal/controlclient"
	"github.com/torana-edge/torana-edge/internal/fileperm"
	"github.com/torana-edge/torana-edge/internal/instance"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func Handles(args []string) bool {
	return len(args) > 0 && (args[0] == "start" || args[0] == "stop" || args[0] == "status" || args[0] == "open" || args[0] == "endpoint")
}

func Usage(w io.Writer) {
	fmt.Fprint(w, `Local process management:
  torana start [--port N] [--bind address] [--timeout 60s]
                                 start a background instance, or report the existing one
  torana status [--addr origin]  inspect this managed store's running instance
  torana open [--addr origin]    open that instance's local control plane
  torana endpoint [provider]     print its live origin or provider endpoint
  torana stop --yes [--addr origin] [--timeout 15s]

start uses the same TORANA_CONFIG, TORANA_DATA_DIR, TORANA_PORT and TORANA_BIND
as serve, and forwards --port and --bind to it. Those flags apply only when
start actually launches an instance; they never re-bind one already running. It starts this binary directly, waits for readiness, and records logs
in TORANA_DATA_DIR/torana.log (the default data directory applies when unset).
It does not install an OS service or change shell configuration. stop requests
graceful shutdown of the inspected instance; it never kills a PID from a file.
Add --json for machine-readable output. Connection errors other than
connection-refused are not treated as proof that a process is stopped.
`)
}

type Status struct {
	Service         string    `json:"service"`
	InstanceID      string    `json:"instance_id,omitempty"`
	PID             int       `json:"pid,omitempty"`
	Version         string    `json:"version,omitempty"`
	Status          string    `json:"status"`
	ConfigPath      string    `json:"config_path,omitempty"`
	Address         string    `json:"address"`
	LogPath         string    `json:"log_path,omitempty"`
	StartedAt       time.Time `json:"started_at,omitzero"`
	UptimeSeconds   int64     `json:"uptime_seconds,omitempty"`
	Port            int       `json:"port,omitempty"`
	PluginDirectory string    `json:"plugin_directory,omitempty"`
}

func Inspect(ctx context.Context, c *controlclient.Client) (Status, error) {
	s := Status{Address: c.Address()}
	raw, _, err := c.JSON(ctx, http.MethodGet, controlclient.BasePath+"/system", nil, "")
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	if s.Service != "torana-edge" || s.InstanceID == "" || s.PID <= 0 || s.ConfigPath == "" {
		return s, fmt.Errorf("endpoint is not an identifiable Torana instance; refusing lifecycle operations")
	}
	s.Address = c.Address()
	return s, nil
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if !Handles(args) {
		return fmt.Errorf("expected start, stop, status, open, or endpoint")
	}
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { Usage(stderr) }
	var addr string
	var yes bool
	timeout := 15 * time.Second
	var serveFlags ServeFlags
	if command == "start" {
		timeout = 60 * time.Second
		fs.StringVar(&serveFlags.Port, "port", "", "listen port for the started instance")
		fs.StringVar(&serveFlags.Bind, "bind", "", "bind address for the started instance")
	} else {
		fs.StringVar(&addr, "addr", "", "loopback control-plane origin")
	}
	jsonOutput := fs.Bool("json", false, "machine-readable JSON output")
	if command == "stop" {
		fs.BoolVar(&yes, "yes", false, "confirm shutdown")
	}
	fs.DurationVar(&timeout, "timeout", timeout, "maximum wait")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if command != "endpoint" && fs.NArg() != 0 {
		return fmt.Errorf("%s takes no positional arguments", command)
	}
	if command == "endpoint" && fs.NArg() > 1 {
		return fmt.Errorf("endpoint accepts at most one provider name")
	}
	if command == "open" && *jsonOutput {
		return fmt.Errorf("open launches a browser and does not support --json")
	}
	if command == "endpoint" && *jsonOutput {
		return fmt.Errorf("endpoint prints a shell-ready URL and does not support --json")
	}
	if timeout <= 0 || timeout > 10*time.Minute {
		return fmt.Errorf("--timeout must be positive and at most 10m")
	}
	if command == "stop" && !yes {
		return fmt.Errorf("stop shuts down the running proxy; pass --yes")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var s Status
	var err error
	var c *controlclient.Client
	if command == "start" {
		s, err = Start(ctx, serveFlags)
	} else {
		c, err = controlclient.New(addr, 2*time.Second)
		if err != nil {
			return err
		}
		defer c.Close()
		s, err = Inspect(ctx, c)
		if err != nil && connectionRefused(err) {
			if addr == "" {
				path := c.StorePath()
				active, e := instance.Running(filepath.Join(filepath.Dir(path), "instance.lock"))
				if e != nil {
					return e
				}
				if active {
					return fmt.Errorf("Torana owns this store but its API is unavailable (starting, stopping, or failed): %w", err)
				}
			}
			s = Status{Service: "torana-edge", Status: "stopped", Address: c.Address()}
			err = nil
		} else if err == nil {
			if addr == "" {
				path := c.StorePath()
				var e error
				path, e = filepath.Abs(path)
				if e != nil {
					return e
				}
				if filepath.Clean(path) != filepath.Clean(s.ConfigPath) {
					return fmt.Errorf("endpoint belongs to a different managed store; inspect it explicitly with --addr")
				}
			}
			if command == "stop" {
				s, err = stop(ctx, c, s)
			} else if command == "open" {
				if err = validateOpenStatus(s); err == nil {
					err = openControlPlane(s, runtime.GOOS, startBrowser)
				}
			}
		}
	}
	if err != nil {
		return err
	}
	if command == "open" {
		if err := validateOpenStatus(s); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Opened %s/_torana/\n", strings.TrimRight(s.Address, "/"))
		return nil
	}
	if command == "endpoint" {
		if s.Status != "running" {
			return fmt.Errorf("Torana is not running; start it before requesting an endpoint")
		}
		endpoint := strings.TrimRight(s.Address, "/")
		if fs.NArg() == 1 {
			provider := fs.Arg(0)
			if provider == "" || provider == "." || provider == ".." || strings.ContainsAny(provider, "/\\?#") {
				return fmt.Errorf("provider name must be one URL path segment")
			}
			raw, _, readErr := c.JSON(ctx, http.MethodGet, controlclient.BasePath+"/config", nil, "")
			if readErr != nil {
				return readErr
			}
			var config struct {
				Providers map[string]json.RawMessage `json:"providers"`
			}
			if json.Unmarshal(raw, &config) != nil {
				return fmt.Errorf("running instance returned an invalid provider configuration")
			}
			if _, exists := config.Providers[provider]; !exists {
				return fmt.Errorf("provider %q is not configured in the running instance", provider)
			}
			endpoint += "/provider/" + provider
		}
		_, err := fmt.Fprintln(stdout, endpoint)
		return err
	}
	return printStatus(stdout, s, *jsonOutput)
}

func validateOpenStatus(s Status) error {
	if s.Status != "running" {
		return fmt.Errorf("Torana is not running; start it before opening the control plane")
	}
	return nil
}

var startBrowser = func(name string, args ...string) error {
	return exec.Command(name, args...).Start()
}

func openControlPlane(s Status, goos string, start func(string, ...string) error) error {
	url := strings.TrimRight(s.Address, "/") + "/_torana/"
	var name string
	var args []string
	switch goos {
	case "darwin":
		name, args = "open", []string{url}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		name, args = "xdg-open", []string{url}
	}
	if err := start(name, args...); err != nil {
		return fmt.Errorf("could not open a browser; open %s manually: %w", url, err)
	}
	return nil
}

func printStatus(w io.Writer, s Status, jsonOutput bool) error {
	if jsonOutput {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(s)
	}
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(table, "Torana\t%s\n", s.Status)
	if s.Status != "stopped" {
		fmt.Fprintf(table, "Control plane\t%s/_torana/\n", s.Address)
		if s.PID > 0 {
			fmt.Fprintf(table, "PID\t%d\n", s.PID)
		}
		if s.UptimeSeconds > 0 {
			fmt.Fprintf(table, "Uptime\t%s\n", time.Duration(s.UptimeSeconds)*time.Second)
		}
	}
	for _, row := range [][2]string{{"Address", s.Address}, {"Version", s.Version}, {"Config", s.ConfigPath}, {"Plugins", s.PluginDirectory}, {"Logs", s.LogPath}} {
		if row[1] != "" {
			fmt.Fprintf(table, "%s\t%s\n", row[0], row[1])
		}
	}
	return table.Flush()
}

func stop(ctx context.Context, c *controlclient.Client, s Status) (Status, error) {
	body, _ := json.Marshal(map[string]string{"instance_id": s.InstanceID})
	if _, _, err := c.JSON(ctx, http.MethodPost, controlclient.BasePath+"/system/stop", body, ""); err != nil {
		return s, err
	}
	// Wait for the instance to disappear, not merely a 202 response. For our
	// store, also wait for its OS lock: HTTP closes before plugin drain finishes.
	store := c.StorePath()
	if store == "" {
		var err error
		store, err = provider.ManagedStorePath()
		if err != nil {
			return s, err
		}
	}
	store, err := filepath.Abs(store)
	if err != nil {
		return s, err
	}
	for {
		current, err := Inspect(ctx, c)
		if err == nil && current.InstanceID != s.InstanceID {
			return s, fmt.Errorf("the requested instance stopped, but another instance is now running; it was not stopped")
		}
		if connectionRefused(err) {
			active := false
			if filepath.Clean(store) == filepath.Clean(s.ConfigPath) {
				active, err = instance.Running(filepath.Join(filepath.Dir(store), "instance.lock"))
				if err != nil {
					return s, err
				}
			}
			if !active {
				s.Status = "stopped"
				return s, nil
			}
		} else if err != nil {
			return s, fmt.Errorf("shutdown requested, but could not verify completion: %w", err)
		}
		if err := pause(ctx); err != nil {
			return s, fmt.Errorf("shutdown requested; inspect status before retrying: %w", err)
		}
	}
}

// Start is idempotent for this managed store. A start lock serializes launchers;
// the child's lifetime lock also excludes foreground serve and port overrides.
// ServeFlags are listener settings forwarded verbatim to the instance start
// launches. They exist so `torana start --port 9090` means the same thing as
// `torana serve --port 9090`; without forwarding, start would silently ignore
// them and report a healthy instance on the wrong port.
type ServeFlags struct {
	Port string
	Bind string
}

func (f ServeFlags) args() []string {
	args := []string{"serve"}
	if f.Port != "" {
		args = append(args, "--port", f.Port)
	}
	if f.Bind != "" {
		args = append(args, "--bind", f.Bind)
	}
	return args
}

func Start(ctx context.Context, flags ServeFlags) (Status, error) {
	executable, err := os.Executable()
	if err != nil {
		return Status{}, err
	}
	return startExecutable(ctx, executable, flags)
}

func startExecutable(ctx context.Context, executable string, flags ServeFlags) (Status, error) {
	var zero Status
	store, err := provider.ManagedStorePath()
	if err != nil {
		return zero, err
	}
	store, err = filepath.Abs(store)
	if err != nil {
		return zero, err
	}
	dir := filepath.Dir(store)
	var lock *instance.Lock
	for {
		lock, err = instance.Acquire(filepath.Join(dir, "start.lock"))
		if err == nil {
			break
		}
		if !errors.Is(err, instance.ErrLocked) {
			return zero, err
		}
		if err := pause(ctx); err != nil {
			return zero, err
		}
	}
	defer func() { _ = lock.Close() }()
	active, err := instance.Running(filepath.Join(dir, "instance.lock"))
	if err != nil {
		return zero, err
	}
	if active {
		return waitReady(ctx, store, "", 0, nil)
	}
	// Resolve/validate the endpoint the CHILD will actually listen on, then
	// preflight that one. Resolving without the flags probed the old port while
	// launching on the new one, so `start --port <free>` failed whenever the
	// previously configured port was occupied — exactly the case the flag
	// exists for. Both sides now share controlclient's resolver, so the
	// precedence cannot drift apart again.
	//
	// An explicit but unusable flag still fails here: bypassing preflight would
	// trade a clear refusal for an instance nobody can administer.
	target, err := controlclient.ResolveAddress(controlclient.Listener{Port: flags.Port, Bind: flags.Bind})
	if err != nil {
		return zero, err
	}
	c, err := controlclient.New(target, 2*time.Second)
	if err != nil {
		return zero, err
	}
	_, inspectErr := Inspect(ctx, c)
	c.Close()
	if inspectErr == nil {
		return zero, fmt.Errorf("%s already serves another Torana instance; choose a free --port or inspect it with --addr", target)
	}
	if !connectionRefused(inspectErr) {
		return zero, fmt.Errorf("cannot safely start on %s: %w", target, inspectErr)
	}
	logPath := filepath.Join(dir, "torana.log")
	if info, err := os.Lstat(logPath); err == nil && !info.Mode().IsRegular() {
		return zero, fmt.Errorf("refusing non-regular log file %s", logPath)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return zero, err
	}
	defer func() { _ = logFile.Close() }()
	if err := fileperm.Secure(logPath, logFile); err != nil {
		return zero, err
	}
	cmd := exec.Command(executable, flags.args()...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return zero, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	s, err := waitReady(ctx, store, target, cmd.Process.Pid, done)
	if err != nil {
		// Only the child handle created here is eligible for startup cleanup.
		if killErr := cmd.Process.Kill(); killErr == nil {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		}
		return zero, fmt.Errorf("Torana did not become ready; inspect %s: %w", logPath, err)
	}
	s.LogPath = logPath
	return s, nil
}

func waitReady(ctx context.Context, store, target string, pid int, done <-chan error) (Status, error) {
	var lastProbeError error
	for {
		select {
		case err := <-done:
			return Status{}, fmt.Errorf("child exited before readiness: %v", err)
		default:
		}
		// A launched child already has a resolved listener. Using defaultTarget
		// here probes the instance lock and can race the child acquiring it.
		// Existing-instance waits still discover the recorded runtime listener.
		c, err := controlclient.New(target, time.Second)
		if err == nil {
			s, probeErr := Inspect(ctx, c)
			c.Close()
			if probeErr != nil && ctx.Err() == nil {
				lastProbeError = probeErr
			}
			if probeErr == nil {
				if filepath.Clean(s.ConfigPath) != filepath.Clean(store) || pid != 0 && s.PID != pid {
					return Status{}, fmt.Errorf("endpoint belongs to a different instance")
				}
				if s.Status == "running" {
					return s, nil
				}
				if s.Status == "degraded" {
					return Status{}, fmt.Errorf("instance is running but degraded; inspect plugin status and logs")
				}
			}
		} else if ctx.Err() == nil {
			lastProbeError = err
		}
		if err := pause(ctx); err != nil {
			if lastProbeError != nil {
				return Status{}, fmt.Errorf("%w (last status check: %v)", err, lastProbeError)
			}
			return Status{}, err
		}
	}
}

func pause(ctx context.Context) error {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
