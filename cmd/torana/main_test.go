package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every environment variable Torana reads must appear in the help text.
//
// The env table was previously incomplete, and TORANA_BIND being absent was the
// expensive one: a container started with `docker run -p 8080:8080` answers
// nothing without it, and the symptom — a published port that looks dead — does
// not point at a bind address.
func TestUsageDocumentsEveryEnvironmentVariable(t *testing.T) {
	var buf bytes.Buffer
	usage(&buf)
	help := buf.String()

	for _, name := range []string{
		"TORANA_CONFIG",
		"TORANA_DATA_DIR",
		"TORANA_PORT",
		"TORANA_BIND",
		"TORANA_DEFAULT_PROVIDER",
		"TORANA_PLUGINS_DIR",
	} {
		if !strings.Contains(help, name) {
			t.Errorf("%s is read by Torana but absent from the help text", name)
		}
	}
}

// The help text must name every subcommand main() dispatches, or a command
// exists that nobody can discover.
func TestUsageDocumentsEverySubcommand(t *testing.T) {
	var buf bytes.Buffer
	usage(&buf)
	help := buf.String()

	for _, cmd := range []string{"serve", "plugin", "conversations", "version", "help"} {
		if !strings.Contains(help, cmd) {
			t.Errorf("subcommand %q is dispatched but absent from the help text", cmd)
		}
	}
}

// A container reports its version from a build arg. Without the ldflags stamp
// every image says "dev", and an untagged build deliberately skips the plugin
// product-version compatibility gates — so a released image would silently stop
// enforcing minimum_torana_version.
func TestDockerfileStampsVersionAndBindsAllInterfaces(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(raw)

	if !strings.Contains(dockerfile, "-X main.version=") {
		t.Error("Dockerfile does not stamp main.version — every image would report \"dev\" " +
			"and skip product-version compatibility gates")
	}
	if !strings.Contains(dockerfile, "TORANA_BIND=0.0.0.0") {
		t.Error("Dockerfile does not set TORANA_BIND — a published port would answer nothing")
	}
}
