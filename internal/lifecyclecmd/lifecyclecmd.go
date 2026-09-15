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
	"text/tabwriter"
	"time"

	"github.com/torana-edge/torana-edge/internal/controlclient"
	"github.com/torana-edge/torana-edge/internal/fileperm"
	"github.com/torana-edge/torana-edge/internal/instance"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func Handles(args []string) bool {
	return len(args) > 0 && (args[0] == "start" || args[0] == "stop" || args[0] == "status")
}

func Usage(w io.Writer) {
	fmt.Fprint(w, `Local process management:
  torana start [--timeout 60s]    start a background instance, or report the existing one
  torana status [--addr origin]  inspect this managed store's running instance
  torana stop --yes [--addr origin] [--timeout 15s]

start uses the same TORANA_CONFIG, TORANA_DATA_DIR, TORANA_PORT and TORANA_BIND
as serve. It starts this binary directly, waits for readiness, and records logs
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
		return fmt.Errorf("expected start, stop, or status")
	}
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { Usage(stderr) }
	var addr string
	var yes bool
	timeout := 15 * time.Second
	if command == "start" {
		timeout = 60 * time.Second
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
	if fs.NArg() != 0 {
		return fmt.Errorf("%s takes no positional arguments", command)
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
	if command == "start" {
		s, err = Start(ctx)
	} else {
		var c *controlclient.Client
		c, err = controlclient.New(addr, 2*time.Second)
		if err != nil {
			return err
		}
		defer c.Close()
		s, err = Inspect(ctx, c)
		if err != nil && connectionRefused(err) {
			if addr == "" {
				path, e := provider.ManagedStorePath()
				if e != nil {
					return e
				}
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
				path, e := provider.ManagedStorePath()
				if e != nil {
					return e
				}
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
			}
		}
	}
	if err != nil {
		return err
	}
	return printStatus(stdout, s, *jsonOutput)
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
	store, err := provider.ManagedStorePath()
	if err != nil {
		return s, err
	}
	store, err = filepath.Abs(store)
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
func Start(ctx context.Context) (Status, error) {
	executable, err := os.Executable()
	if err != nil {
		return Status{}, err
	}
	return startExecutable(ctx, executable)
}

func startExecutable(ctx context.Context, executable string) (Status, error) {
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
		return waitReady(ctx, store, 0, nil)
	}
	// Resolve/validate the intended control address before spawning. A daemon
	// with no loopback API cannot be managed by these commands.
	c, err := controlclient.New("", 2*time.Second)
	if err != nil {
		return zero, err
	}
	_, inspectErr := Inspect(ctx, c)
	c.Close()
	if inspectErr == nil {
		return zero, fmt.Errorf("the requested port already serves another Torana instance; choose TORANA_PORT or inspect --addr")
	}
	if !connectionRefused(inspectErr) {
		return zero, fmt.Errorf("cannot safely start on the requested endpoint: %w", inspectErr)
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
	cmd := exec.Command(executable, "serve")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return zero, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	s, err := waitReady(ctx, store, cmd.Process.Pid, done)
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

func waitReady(ctx context.Context, store string, pid int, done <-chan error) (Status, error) {
	var lastProbeError error
	for {
		select {
		case err := <-done:
			return Status{}, fmt.Errorf("child exited before readiness: %v", err)
		default:
		}
		c, err := controlclient.New("", time.Second)
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
