package main

import (
	"bytes"
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
