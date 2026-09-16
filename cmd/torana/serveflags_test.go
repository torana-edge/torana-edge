package main

import (
	"io"
	"strings"
	"testing"
)

// serve used to ignore everything typed after it, so `torana serve --port 9090`
// listened on the configured port and reported success. These cases pin the
// parse, the refusals, and the "not given" zero values the precedence relies on.
func TestParseServeFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		port int
		bind string
	}{
		{"no flags leaves both unset", nil, 0, ""},
		{"port only", []string{"--port", "9090"}, 9090, ""},
		{"bind only", []string{"--bind", "0.0.0.0"}, 0, "0.0.0.0"},
		{"both", []string{"--port", "1", "--bind", "::1"}, 1, "::1"},
		{"highest valid port", []string{"--port", "65535"}, 65535, ""},
		{"single dash is accepted by flag", []string{"-port", "8143"}, 8143, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := parseServeFlags(tc.args, io.Discard)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if opts.port != tc.port || opts.bind != tc.bind {
				t.Fatalf("got port=%d bind=%q, want port=%d bind=%q", opts.port, opts.bind, tc.port, tc.bind)
			}
		})
	}
}

func TestParseServeFlagsRefusesUnusableValues(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		args       []string
	}{
		{"not a number", "is not a number", []string{"--port", "808O"}},
		{"zero", "outside the valid port range", []string{"--port", "0"}},
		{"negative", "outside the valid port range", []string{"--port", "-1"}},
		{"above range", "outside the valid port range", []string{"--port", "65536"}},
		{"positional argument", "takes no positional arguments", []string{"9090"}},
		{"unknown flag", "flag provided but not defined", []string{"--listen", "9090"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseServeFlags(tc.args, io.Discard); err == nil {
				t.Fatal("expected an error")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A malformed port must not fall back to the configured one. Reporting success
// on a port the operator did not ask for is the failure this refuses.
func TestParseServeFlagsReportsNoPortWhenItRefuses(t *testing.T) {
	opts, err := parseServeFlags([]string{"--port", "70000"}, io.Discard)
	if err == nil {
		t.Fatal("expected an error")
	}
	if opts.port != 0 || opts.bind != "" {
		t.Fatalf("a refused parse must yield no settings, got %+v", opts)
	}
}
