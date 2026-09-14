package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestBinaryBackgroundLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and starts a real isolated binary")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "torana")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	data := filepath.Join(root, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := provider.DefaultConfig()
	// Deliberately persist a DIFFERENT port. Status from another shell must
	// find the daemon's runtime override through its instance record.
	cfg.Port = 1
	cfg.Plugins.Dir = filepath.Join(root, "plugins")
	if err := os.Mkdir(cfg.Plugins.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(filepath.Join(data, "config.json"), cfg); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "TORANA_") && !strings.HasPrefix(value, "OTEL_") {
			env = append(env, value)
		}
	}
	env = append(env, "TORANA_DATA_DIR="+data, "TORANA_BIND=127.0.0.1")
	run := func(override bool, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = append([]string(nil), env...)
		if override {
			cmd.Env = append(cmd.Env, "TORANA_PORT="+strconv.Itoa(port))
		}
		return cmd.CombinedOutput()
	}
	started, err := run(true, "start", "--timeout", "20s")
	if err != nil {
		t.Fatalf("start: %v\n%s", err, started)
	}
	t.Cleanup(func() { _, _ = run(false, "stop", "--yes", "--timeout", "10s") })
	var initial map[string]any
	if err := json.Unmarshal(started, &initial); err != nil {
		t.Fatalf("start not JSON: %s", started)
	}
	for _, command := range []string{"status", "start"} {
		out, err := run(false, command)
		if err != nil {
			t.Fatalf("%s: %v\n%s", command, err, out)
		}
		var current map[string]any
		if err := json.Unmarshal(out, &current); err != nil {
			t.Fatal(err)
		}
		if current["instance_id"] != initial["instance_id"] {
			t.Fatalf("%s did not find original instance: %s", command, out)
		}
	}
	if out, err := run(true, "serve"); err == nil || !strings.Contains(string(out), "another Torana process") {
		t.Fatalf("duplicate serve: %v %s", err, out)
	}
	if out, err := run(false, "stop", "--yes"); err != nil || !strings.Contains(string(out), `"status":"stopped"`) {
		t.Fatalf("stop: %v %s", err, out)
	}
	if out, err := run(true, "status"); err != nil || !strings.Contains(string(out), `"status":"stopped"`) {
		t.Fatalf("after stop: %v %s", err, out)
	}
}
