package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Which rules a config had to pass used to depend on how it arrived: the
// structural checks ran only on the control-plane PUT, and the per-provider
// checks only when merging an unmanaged seed. The managed store — the config
// every running Torana actually uses after first start — was covered by
// neither, despite being the one the control plane writes to.

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestManagedConfigIsValidated is the regression test: each of these was
// accepted without complaint when marked managed.
func TestManagedConfigIsValidated(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"invalid port": {
			`{"managed":true,"port":99999,"providers":{}}`,
			"port",
		},
		"negative limits": {
			`{"managed":true,"port":8080,"providers":{},"limits":{"rpm":-1}}`,
			"limits",
		},
		"provider with a non-http url": {
			`{"managed":true,"port":8080,"providers":{"p":{"url":"ftp://x/","format":"openai"}}}`,
			"url",
		},
		"provider with an unsupported format": {
			`{"managed":true,"port":8080,"providers":{"p":{"url":"https://x/","format":"nope"}}}`,
			"format",
		},
		"fallback to a provider that is not configured": {
			`{"managed":true,"port":8080,"providers":{"p":{"url":"https://x/","format":"openai","fallback":["ghost"]}}}`,
			"fallback",
		},
		"provider that falls back to itself": {
			`{"managed":true,"port":8080,"providers":{"p":{"url":"https://x/","format":"openai","fallback":["p"]}}}`,
			"itself",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("managed config accepted without validation: %s", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestValidManagedConfigStillLoads guards the other direction: tightening
// validation must not reject configs that are fine.
func TestValidManagedConfigStillLoads(t *testing.T) {
	cfg, err := Load(writeConfig(t,
		`{"managed":true,"port":8080,"providers":{"openai":{"url":"https://api.openai.com/v1","format":"openai"}}}`))
	if err != nil {
		t.Fatalf("valid managed config rejected: %v", err)
	}
	if cfg.Port != 8080 || len(cfg.Providers) != 1 {
		t.Errorf("config misloaded: %+v", cfg)
	}
}

// TestMissingConfigIsNotAnError is the check that keeps failing closed safe.
// main.go now aborts when ResolveConfig returns an error, so a fresh install
// with no config must NOT produce one — otherwise first run breaks for
// everybody.
func TestMissingConfigIsNotAnError(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("a missing config must resolve to defaults, not an error: %v", err)
	}
	if cfg.Port == 0 {
		t.Error("defaults did not populate a port")
	}
}

// TestDefaultConfigPassesValidation guards the same invariant from the other side: the
// defaults a fresh install starts from must themselves pass validation, or
// first run aborts on a config the user never wrote.
func TestDefaultConfigPassesValidation(t *testing.T) {
	if err := validate(DefaultConfig()); err != nil {
		t.Fatalf("DefaultConfig does not pass its own validation: %v", err)
	}
}

// TestResolveConfigMaterializesFreshInstall walks the actual first-run path:
// no seed, no managed store. It must succeed and write the store.
func TestResolveConfigMaterializesFreshInstall(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "managed", "config.json")

	cfg, err := ResolveConfig(filepath.Join(dir, "no-seed.json"), storePath)
	if err != nil {
		t.Fatalf("fresh install failed to resolve: %v", err)
	}
	if !cfg.Managed {
		t.Error("resolved config should be marked managed after materializing")
	}
	if _, err := os.Stat(storePath); err != nil {
		t.Errorf("managed store was not materialized: %v", err)
	}
}

// TestResolveConfigRejectsBrokenConfig is what main.go now aborts on: the
// config exists and is broken, so defaults would silently drop the user's
// providers and plugin configuration.
func TestResolveConfigRejectsBrokenConfig(t *testing.T) {
	dir := t.TempDir()
	seed := writeConfig(t, `{"port":8080,`) // truncated JSON

	if _, err := ResolveConfig(seed, filepath.Join(dir, "store.json")); err == nil {
		t.Fatal("a malformed config must be an error, not a silent downgrade to defaults")
	}
}
