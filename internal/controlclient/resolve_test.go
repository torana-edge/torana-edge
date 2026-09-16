package controlclient

import (
	"path/filepath"
	"strings"
	"testing"
)

// The launcher and the clients that talk to an instance must resolve the same
// endpoint. Resolving the preflight target without the requested listener meant
// `start --port <free>` probed the previously configured port, so an occupied
// old port refused the very command that exists to move off it.
func TestResolveAddressPrefersExplicitOverEnvironment(t *testing.T) {
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	for _, tc := range []struct {
		name      string
		requested Listener
		port      string
		bind      string
		want      string
	}{
		{"explicit port over environment", Listener{Port: "8231"}, "9999", "", "127.0.0.1:8231"},
		{"explicit port over unusable environment", Listener{Port: "8232"}, "not-a-port", "", "127.0.0.1:8232"},
		{"explicit bind over non-loopback environment", Listener{Port: "8233", Bind: "127.0.0.1"}, "", "192.0.2.1", "127.0.0.1:8233"},
		{"explicit IPv6 loopback", Listener{Port: "8234", Bind: "::1"}, "", "", "[::1]:8234"},
		{"environment still applies when unset", Listener{}, "8235", "", "127.0.0.1:8235"},
		{"localhost bind is accepted", Listener{Port: "8236", Bind: "localhost"}, "", "", "localhost:8236"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TORANA_PORT", tc.port)
			t.Setenv("TORANA_BIND", tc.bind)
			got, err := ResolveAddress(tc.requested)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// An explicit flag that cannot work must fail rather than fall through to the
// environment: bypassing preflight would trade a clear refusal for an instance
// nobody can administer.
func TestResolveAddressRefusesUnusableExplicitValues(t *testing.T) {
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	t.Setenv("TORANA_PORT", "8080")
	t.Setenv("TORANA_BIND", "127.0.0.1")
	for _, tc := range []struct {
		name      string
		requested Listener
		want      string
	}{
		{"port out of range", Listener{Port: "70000"}, "--port"},
		{"port not a number", Listener{Port: "80o80"}, "--port"},
		{"non-loopback bind", Listener{Port: "8080", Bind: "192.0.2.1"}, "--bind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveAddress(tc.requested)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q should name %s, not the environment", err, tc.want)
			}
		})
	}
}

// Without an explicit listener the environment keeps its own error attribution.
func TestResolveAddressStillNamesTheEnvironment(t *testing.T) {
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	t.Setenv("TORANA_PORT", "not-a-port")
	if _, err := ResolveAddress(Listener{}); err == nil || !strings.Contains(err.Error(), "TORANA_PORT") {
		t.Fatalf("want a TORANA_PORT error, got %v", err)
	}
	t.Setenv("TORANA_PORT", "8080")
	t.Setenv("TORANA_BIND", "192.0.2.1")
	if _, err := ResolveAddress(Listener{}); err == nil || !strings.Contains(err.Error(), "TORANA_BIND") {
		t.Fatalf("want a TORANA_BIND error, got %v", err)
	}
	_ = filepath.Join
}
