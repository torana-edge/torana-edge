package provider

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every route the README's front-page diagram advertises must exist on a fresh
// install. /provider/gemini/ was listed there and defined nowhere, so the
// headline example 502'd for anyone who followed it.
func TestDefaultConfigServesEveryAdvertisedRoute(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	defaults := DefaultConfig().Providers

	var advertised []string
	for _, line := range strings.Split(string(readme), "\n") {
		i := strings.Index(line, "/provider/")
		if i < 0 {
			continue
		}
		rest := line[i+len("/provider/"):]
		name, _, ok := strings.Cut(rest, "/")
		if !ok || name == "" || strings.ContainsAny(name, "<>{}") {
			continue // a placeholder like /provider/<name>/, not a real route
		}
		advertised = append(advertised, name)
	}
	if len(advertised) == 0 {
		t.Fatal("no /provider/<name>/ routes found in README.md; this check has stopped " +
			"reading what it is supposed to guard")
	}
	for _, name := range advertised {
		if _, ok := defaults[name]; !ok {
			t.Errorf("README advertises /provider/%s/ but DefaultConfig has no %q provider — "+
				"a fresh install 502s on the documented example", name, name)
		}
	}
}

// Every provider the defaults ship must be one the proxy can actually route.
func TestDefaultConfigProvidersAreValid(t *testing.T) {
	for name, p := range DefaultConfig().Providers {
		if p.URL == "" {
			t.Errorf("default provider %q has no URL", name)
		}
		if _, ok := supportedFormats[p.Format]; !ok {
			t.Errorf("default provider %q declares format %q, which is not supported (%s)",
				name, p.Format, supportedFormatNames())
		}
	}
}

// shippedConfigFiles is the repository-owned inventory of example configs.
//
// It is an explicit list, not a glob. A glob over config*.json also matches the
// operator's own untracked config.json — the file README tells every developer
// to create — so a contributor drafting or deliberately breaking their local
// configuration would fail the repository's test suite with it. Membership of
// this list must not be expandable by a file that is not checked in.
var shippedConfigFiles = []string{"config.example.json"}

// A shipped config file the product refuses to load is a trap: the operator
// copies it, and the binary rejects it with no hint that the file was never
// loadable in the first place.
func TestShippedConfigFilesLoad(t *testing.T) {
	for _, name := range shippedConfigFiles {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(filepath.Join("..", "..", name)); err != nil {
				t.Errorf("%s is shipped in the repository but the product cannot load it: %v",
					name, err)
			}
		})
	}
}

// The inventory above must cover every config JSON the repository actually
// tracks, so a new example added tomorrow is checked rather than forgotten.
// git is the authority on what is tracked; an ignored local file is invisible
// to it, which is exactly the property a glob lacked.
func TestShippedConfigInventoryCoversEveryTrackedConfig(t *testing.T) {
	out, err := exec.Command("git", "-C", filepath.Join("..", ".."), "ls-files", "config*.json").Output()
	if err != nil {
		t.Skipf("git is unavailable, so the tracked inventory cannot be confirmed here: %v", err)
	}
	listed := make(map[string]bool, len(shippedConfigFiles))
	for _, name := range shippedConfigFiles {
		listed[name] = true
	}
	var tracked int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		tracked++
		if !listed[name] {
			t.Errorf("%s is tracked in the repository but missing from shippedConfigFiles, "+
				"so nothing checks that the product can load it", name)
		}
	}
	if tracked == 0 {
		t.Error("git reports no tracked config*.json; this check has stopped seeing what it guards")
	}
	for name := range listed {
		if !strings.Contains(string(out), name) {
			t.Errorf("shippedConfigFiles names %s, which the repository does not track", name)
		}
	}
}

// The binding validator must name the field that is wrong. A single
// ten-condition check reported "an invalid binding or budget", which is
// startup-fatal and tells an operator nothing about which value to change.
func TestModelServiceBindingErrorsNameTheField(t *testing.T) {
	good := PluginModelServiceApproval{
		Provider: "p", Model: "m", Path: "/v1/chat/completions",
		TimeoutMS: 5000, MaxTokens: 256, MaxInputBytes: 1 << 20,
		MaxCallsPerMinute: 10, MaxTokensPerHour: 1000,
	}
	if err := validateModelServiceBinding("pl", "res", good); err != nil {
		t.Fatalf("a valid binding was rejected: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*PluginModelServiceApproval)
		want   string
	}{
		{"empty model", func(b *PluginModelServiceApproval) { b.Model = "  " }, "model"},
		{"bad path", func(b *PluginModelServiceApproval) { b.Path = "not a path" }, "path"},
		{"timeout zero", func(b *PluginModelServiceApproval) { b.TimeoutMS = 0 }, "timeout_ms"},
		{"timeout too large", func(b *PluginModelServiceApproval) { b.TimeoutMS = 120001 }, "timeout_ms"},
		{"max_tokens zero", func(b *PluginModelServiceApproval) { b.MaxTokens = 0 }, "max_tokens"},
		{"input bytes zero", func(b *PluginModelServiceApproval) { b.MaxInputBytes = 0 }, "max_input_bytes"},
		{"input bytes too large", func(b *PluginModelServiceApproval) { b.MaxInputBytes = 9 << 20 }, "max_input_bytes"},
		{"calls per minute zero", func(b *PluginModelServiceApproval) { b.MaxCallsPerMinute = 0 }, "max_calls_per_minute"},
		{"tokens per hour zero", func(b *PluginModelServiceApproval) { b.MaxTokensPerHour = 0 }, "max_tokens_per_hour"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := good
			tc.mutate(&b)
			err := validateModelServiceBinding("pl", "res", b)
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the offending field %q", err, tc.want)
			}
		})
	}
}
