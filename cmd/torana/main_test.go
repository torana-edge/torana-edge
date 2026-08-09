package main

import (
	"bytes"
	"runtime/debug"
	"strings"
	"testing"
)

func TestVersionFromBuildInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fallback string
		info     *debug.BuildInfo
		ok       bool
		want     string
	}{
		{name: "linked release wins", fallback: "v0.1.0", info: &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}}, ok: true, want: "v0.1.0"},
		{name: "tagged go install", fallback: "dev", info: &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}, ok: true, want: "v0.1.0"},
		{name: "pseudo version is visible", fallback: "dev", info: &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20260808065220-fb5ce695e2d4"}}, ok: true, want: "v0.0.0-20260808065220-fb5ce695e2d4"},
		{name: "local build", fallback: "dev", info: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, ok: true, want: "dev"},
		{name: "missing info", fallback: "dev", info: nil, ok: false, want: "dev"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := versionFromBuildInfo(tt.fallback, tt.info, tt.ok)
			if got != tt.want {
				t.Fatalf("version = %q, want %q", got, tt.want)
			}
		})
	}
}

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

// main() refuses to start when the pinned SDK's outbound field-policy
// registry is broken, because a broken policy table makes every verifier
// decision garbage. This exercises the exact function the serve path calls at
// startup; it fails when the SDK ships an invalid registry (the failure mode
// the check exists to surface loudly).
func TestValidateOutboundPolicy(t *testing.T) {
	if err := validateOutboundPolicy(); err != nil {
		t.Fatalf("outbound policy registry invalid: %v", err)
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
