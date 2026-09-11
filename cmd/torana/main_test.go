package main

import (
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

// TORANA_PORT is a first-hour failure mode: a typo used to be swallowed, so
// the proxy listened on the config's port while the operator believed the
// override had taken. The decision lives in parsePortOverride precisely so it
// can be pinned here rather than depending on main staying the way it is.
func TestParsePortOverride(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int
		wantErr bool
	}{
		{in: "8080", want: 8080},
		{in: "1", want: 1},         // lowest valid port
		{in: "65535", want: 65535}, // highest valid port
		{in: "0", wantErr: true},
		{in: "65536", wantErr: true},
		{in: "-1", wantErr: true},
		{in: "808O", wantErr: true}, // letter O, the typo that started this
		{in: "", wantErr: true},
		{in: "8080 ", wantErr: true},
		{in: "80.80", wantErr: true},
		{in: "0x1f90", wantErr: true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parsePortOverride(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("TORANA_PORT=%q was accepted as port %d; a malformed or "+
						"out-of-range override must be refused, not ignored", tc.in, got)
				}
				if !strings.Contains(err.Error(), "TORANA_PORT") {
					t.Errorf("error %q does not name the variable the operator set", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("TORANA_PORT=%q was rejected: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("port = %d, want %d", got, tc.want)
			}
		})
	}
}
