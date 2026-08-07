package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSaveAndLoadManagedConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")

	cfg := Config{
		Port: 9090,
		Providers: map[string]Provider{
			"openai": {
				URL:    "https://custom.openai.com",
				Format: "openai",
			},
		},
		Plugins: PluginsConfig{
			Dir:   "./custom-plugins",
			Order: []string{"intent", "schema_translator"},
			Config: map[string]json.RawMessage{
				"intent": json.RawMessage(`{"mode":"strict"}`),
			},
		},
	}

	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if !loaded.Managed {
		t.Errorf("expected loaded.Managed to be true, got false")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("config directory mode = %o, want 700", got)
	}
	if loaded.Port != 9090 {
		t.Errorf("loaded.Port = %d, want 9090", loaded.Port)
	}
	// "deepseek" default provider should NOT exist because default-merge is skipped for managed config
	if _, ok := loaded.Providers["deepseek"]; ok {
		t.Errorf("expected default provider 'deepseek' to be absent in managed config")
	}
	if len(loaded.Providers) != 1 || loaded.Providers["openai"].URL != "https://custom.openai.com" {
		t.Errorf("unexpected providers in loaded config: %+v", loaded.Providers)
	}
	if !reflect.DeepEqual(loaded.Plugins.Order, []string{"intent", "schema_translator"}) {
		t.Errorf("loaded.Plugins.Order = %v, want [intent schema_translator]", loaded.Plugins.Order)
	}
}

func TestLoadUnmanagedConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	handWrittenJSON := `{
		"port": 7070,
		"providers": {
			"my-custom-provider": {
				"url": "http://localhost:11434",
				"format": "openai"
			}
		}
	}`

	if err := os.WriteFile(path, []byte(handWrittenJSON), 0644); err != nil {
		t.Fatalf("os.WriteFile failed: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if loaded.Managed {
		t.Errorf("expected loaded.Managed to be false for hand-written config")
	}
	if loaded.Port != 7070 {
		t.Errorf("loaded.Port = %d, want 7070", loaded.Port)
	}
	// Defaults should be merged in for unmanaged config
	if _, ok := loaded.Providers["deepseek"]; !ok {
		t.Errorf("expected default provider 'deepseek' to be merged in for unmanaged config")
	}
	if _, ok := loaded.Providers["my-custom-provider"]; !ok {
		t.Errorf("expected custom provider 'my-custom-provider' to be present")
	}
}

func TestManagedStorePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TORANA_DATA_DIR", dir)

	path, err := ManagedStorePath()
	if err != nil {
		t.Fatalf("ManagedStorePath failed: %v", err)
	}
	expected := filepath.Join(dir, "config.json")
	if path != expected {
		t.Errorf("ManagedStorePath = %q, want %q", path, expected)
	}
}

func TestConfigValidateRejectsInvalidProviderGraph(t *testing.T) {
	valid := Config{
		Port: 8080,
		Providers: map[string]Provider{
			"primary": {
				URL:      "https://api.example.test",
				Format:   "openai",
				Fallback: []string{"fallback"},
			},
			"fallback": {
				URL:    "http://127.0.0.1:11434",
				Format: "openai",
			},
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}

	invalid := valid
	invalid.Providers = map[string]Provider{
		"primary": {URL: "file:///tmp/socket", Format: "openai"},
	}
	if err := invalid.Validate(); err == nil {
		t.Fatal("non-http provider URL was accepted")
	}

	invalid = valid
	invalid.Providers = map[string]Provider{
		"primary": {URL: "https://api.example.test", Format: "openai", Fallback: []string{"missing"}},
	}
	if err := invalid.Validate(); err == nil {
		t.Fatal("missing fallback provider was accepted")
	}
}

func TestResolveConfig(t *testing.T) {
	t.Run("no store, seed present -> store created with seed values, seed unchanged", func(t *testing.T) {
		dir := t.TempDir()
		seedPath := filepath.Join(dir, "seed.json")
		storePath := filepath.Join(dir, "data", "config.json")

		seedJSON := `{
			"port": 9090,
			"providers": {
				"custom": {
					"url": "http://localhost:11434",
					"format": "openai"
				}
			}
		}`
		if err := os.WriteFile(seedPath, []byte(seedJSON), 0644); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		cfg, err := ResolveConfig(seedPath, storePath)
		if err != nil {
			t.Fatalf("ResolveConfig failed: %v", err)
		}

		// Store created with seed values + defaults
		if cfg.Port != 9090 {
			t.Errorf("cfg.Port = %d, want 9090", cfg.Port)
		}
		if _, ok := cfg.Providers["custom"]; !ok {
			t.Errorf("expected provider 'custom' to be present")
		}
		if _, ok := cfg.Providers["deepseek"]; !ok {
			t.Errorf("expected default provider 'deepseek' to be merged from seed load")
		}
		if !cfg.Managed {
			t.Errorf("expected cfg.Managed to be true")
		}

		// Verify store file exists on disk and is marked managed
		storeLoaded, err := Load(storePath)
		if err != nil {
			t.Fatalf("Load(storePath) failed: %v", err)
		}
		if !storeLoaded.Managed {
			t.Errorf("expected store file on disk to have Managed: true")
		}

		// Verify seed file is unchanged
		seedContent, err := os.ReadFile(seedPath)
		if err != nil {
			t.Fatalf("ReadFile(seedPath) failed: %v", err)
		}
		if string(seedContent) != seedJSON {
			t.Errorf("seed file was modified; got %q, want %q", string(seedContent), seedJSON)
		}
	})

	t.Run("store present -> seed ignored, store returned", func(t *testing.T) {
		dir := t.TempDir()
		seedPath := filepath.Join(dir, "seed.json")
		storePath := filepath.Join(dir, "config.json")

		seedJSON := `{"port": 9090}`
		storeJSON := `{"managed": true, "port": 7070, "providers": {"store-provider": {"url": "http://store", "format": "openai"}}}`

		if err := os.WriteFile(seedPath, []byte(seedJSON), 0644); err != nil {
			t.Fatalf("WriteFile seed failed: %v", err)
		}
		if err := os.WriteFile(storePath, []byte(storeJSON), 0644); err != nil {
			t.Fatalf("WriteFile store failed: %v", err)
		}

		cfg, err := ResolveConfig(seedPath, storePath)
		if err != nil {
			t.Fatalf("ResolveConfig failed: %v", err)
		}

		if cfg.Port != 7070 {
			t.Errorf("cfg.Port = %d, want 7070", cfg.Port)
		}
		if _, ok := cfg.Providers["store-provider"]; !ok {
			t.Errorf("expected store-provider to be present")
		}
		if _, ok := cfg.Providers["deepseek"]; ok {
			t.Errorf("expected default provider 'deepseek' to NOT be merged into existing managed store")
		}
	})

	t.Run("neither present -> store created from defaults", func(t *testing.T) {
		dir := t.TempDir()
		seedPath := filepath.Join(dir, "nonexistent_seed.json")
		storePath := filepath.Join(dir, "data_dir", "config.json")

		cfg, err := ResolveConfig(seedPath, storePath)
		if err != nil {
			t.Fatalf("ResolveConfig failed: %v", err)
		}

		if cfg.Port != 8080 {
			t.Errorf("cfg.Port = %d, want 8080 (default)", cfg.Port)
		}
		if _, ok := cfg.Providers["deepseek"]; !ok {
			t.Errorf("expected default provider 'deepseek' to be present")
		}
		if !cfg.Managed {
			t.Errorf("expected cfg.Managed to be true")
		}

		if _, err := os.Stat(storePath); err != nil {
			t.Errorf("expected storePath %q to exist on disk, got error: %v", storePath, err)
		}
	})
}

func TestManagedStoreShadowsSeed(t *testing.T) {
	t.Run("materialized seed is equivalent", func(t *testing.T) {
		dir := t.TempDir()
		seedPath := filepath.Join(dir, "seed.json")
		storePath := filepath.Join(dir, "managed", "config.json")
		if err := os.WriteFile(seedPath, []byte(`{"port": 9090}`), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveConfig(seedPath, storePath); err != nil {
			t.Fatal(err)
		}
		differs, err := ManagedStoreShadowsSeed(seedPath, storePath)
		if err != nil {
			t.Fatal(err)
		}
		if differs {
			t.Fatal("freshly materialized store should equal its seed")
		}
	})

	t.Run("changed seed is shadowed", func(t *testing.T) {
		dir := t.TempDir()
		seedPath := filepath.Join(dir, "seed.json")
		storePath := filepath.Join(dir, "managed", "config.json")
		if err := os.WriteFile(seedPath, []byte(`{"port": 9090}`), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveConfig(seedPath, storePath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(seedPath, []byte(`{"port": 7070}`), 0644); err != nil {
			t.Fatal(err)
		}
		differs, err := ManagedStoreShadowsSeed(seedPath, storePath)
		if err != nil {
			t.Fatal(err)
		}
		if !differs {
			t.Fatal("changed seed should differ from managed store")
		}
	})

	t.Run("missing seed is not a conflict", func(t *testing.T) {
		dir := t.TempDir()
		storePath := filepath.Join(dir, "config.json")
		if err := Save(storePath, DefaultConfig()); err != nil {
			t.Fatal(err)
		}
		differs, err := ManagedStoreShadowsSeed(filepath.Join(dir, "missing.json"), storePath)
		if err != nil {
			t.Fatal(err)
		}
		if differs {
			t.Fatal("missing seed should not be reported as shadowed")
		}
	})
}

// writeSeed writes an unmanaged config and loads it.
func writeSeed(t *testing.T, body string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// A plugins stanza without `dir` used to be dropped in its entirety, because
// the merge inferred "did the user provide this section?" from whether one
// sentinel field was non-empty. `approvals` going missing is the
// security-relevant case: the operator's grants simply disappear.
func TestSeedKeepsPluginsWithoutDir(t *testing.T) {
	cfg := writeSeed(t, `{
		"plugins": {
			"order": ["intent", "compactor"],
			"allow_unapproved": false,
			"approvals": {
				"torana/pii": {"digest": "abc123", "permissions": ["env.block_request"]}
			}
		}
	}`)

	if !reflect.DeepEqual(cfg.Plugins.Order, []string{"intent", "compactor"}) {
		t.Errorf("Plugins.Order = %v, want [intent compactor]", cfg.Plugins.Order)
	}
	approval, ok := cfg.Plugins.Approvals["torana/pii"]
	if !ok {
		t.Fatal("operator approvals were dropped — the plugin would not load")
	}
	if approval.Digest != "abc123" {
		t.Errorf("approval digest = %q, want abc123", approval.Digest)
	}
}

// The shipped example's own MITM stanza has enabled:false. Applying the section
// only when enabled was true meant copying config.example.json and starting
// once permanently lost listen, ca_dir and hosts — and nothing reported it,
// because the seed is never re-read and the drift check compares two
// post-merge values.
func TestSeedKeepsDisabledMITM(t *testing.T) {
	cfg := writeSeed(t, `{
		"mitm": {
			"enabled": false,
			"listen": "127.0.0.1:8099",
			"ca_dir": "./local/mitm",
			"hosts": {"cloudcode-pa.googleapis.com": "antigravity"}
		}
	}`)

	if cfg.MITM.Enabled {
		t.Error("MITM.Enabled should stay false")
	}
	if cfg.MITM.Listen != "127.0.0.1:8099" {
		t.Errorf("MITM.Listen = %q — the stanza was dropped", cfg.MITM.Listen)
	}
	if cfg.MITM.CADir != "./local/mitm" {
		t.Errorf("MITM.CADir = %q — the stanza was dropped", cfg.MITM.CADir)
	}
	if cfg.MITM.Hosts["cloudcode-pa.googleapis.com"] != "antigravity" {
		t.Errorf("MITM.Hosts = %v — the stanza was dropped", cfg.MITM.Hosts)
	}
}

// A section the user did not write must keep its default, or every omitted
// section would be zeroed.
func TestSeedOmittedSectionsKeepDefaults(t *testing.T) {
	base := DefaultConfig()
	cfg := writeSeed(t, `{"port": 7070}`)

	if cfg.Port != 7070 {
		t.Errorf("Port = %d, want 7070", cfg.Port)
	}
	if len(cfg.Providers) != len(base.Providers) {
		t.Errorf("providers = %d, want the %d defaults", len(cfg.Providers), len(base.Providers))
	}
	if !reflect.DeepEqual(cfg.MITM, base.MITM) {
		t.Errorf("MITM = %+v, want the default %+v", cfg.MITM, base.MITM)
	}
	if !reflect.DeepEqual(cfg.Plugins, base.Plugins) {
		t.Errorf("Plugins = %+v, want the default %+v", cfg.Plugins, base.Plugins)
	}
}

// Explicitly zeroing a section must be honoured, which is the whole point of
// deciding presence from the JSON rather than from the decoded value.
func TestSeedHonoursExplicitlyEmptySection(t *testing.T) {
	cfg := writeSeed(t, `{"plugins": {"order": []}}`)
	if len(cfg.Plugins.Order) != 0 {
		t.Errorf("Plugins.Order = %v, want empty", cfg.Plugins.Order)
	}
}

// Zero port keeps the default, matching the managed path. This change is about
// sections dropped wholesale, not about tightening scalars.
func TestSeedZeroPortFallsBackToDefault(t *testing.T) {
	cfg := writeSeed(t, `{"port": 0}`)
	if cfg.Port != DefaultConfig().Port {
		t.Errorf("Port = %d, want the default %d", cfg.Port, DefaultConfig().Port)
	}
}

// The shipped example must survive a round trip through the merge, since
// "copy it and run" is the documented first step.
func TestShippedExampleSurvivesTheMerge(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Skipf("config.example.json not readable: %v", err)
	}
	var want Config
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("config.example.json does not parse: %v", err)
	}

	cfg := writeSeed(t, string(raw))

	if !reflect.DeepEqual(cfg.MITM, want.MITM) {
		t.Errorf("MITM lost in merge:\n got %+v\nwant %+v", cfg.MITM, want.MITM)
	}
	if !reflect.DeepEqual(cfg.Plugins, want.Plugins) {
		t.Errorf("Plugins lost in merge:\n got %+v\nwant %+v", cfg.Plugins, want.Plugins)
	}
}

// The README's first step is `cp config.example.json config.json`, so the
// example has to describe a Torana that can actually start. It shipped
// order: ["schema_translator", "intent"] while the repo ships no bundles, and
// the pipeline builds with Strict: true — naming an absent plugin is a hard
// failure, deliberately, so a missing plugin is never silently ignored. The two
// together meant the documented first run died on startup.
func TestShippedExampleNamesNoPluginsItCannotLoad(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Skipf("config.example.json not readable: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config.example.json does not parse: %v", err)
	}

	if len(cfg.Plugins.Order) != 0 {
		t.Errorf("plugins.order = %v — the example must not enable plugins whose "+
			"bundles it does not ship, or copying it and starting fails", cfg.Plugins.Order)
	}
}

// The example is a SEED: it is read once, copied into the managed store, and
// never consulted again. A comment telling the reader to come back and edit it
// after approving plugins describes something that has no effect, and the
// symptom — an edit that changes nothing — is confusing enough that it has to
// be said in the file itself.
func TestShippedExampleExplainsItIsReadOnlyOnce(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Skipf("config.example.json not readable: %v", err)
	}
	var doc struct {
		Plugins struct {
			Comment string `json:"_comment"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("config.example.json does not parse: %v", err)
	}

	comment := strings.ToLower(doc.Plugins.Comment)
	// The managed store is a FILE inside the data directory. Naming
	// $TORANA_DATA_DIR alone sends the reader to a directory and leaves them to
	// guess, which is the kind of near-miss that wastes an afternoon.
	if strings.Contains(comment, "$torana_data_dir") &&
		!strings.Contains(comment, "$torana_data_dir/config.json") {
		t.Error("the comment names $TORANA_DATA_DIR without /config.json — the managed " +
			"store is a file inside that directory, not the directory itself")
	}
	for _, want := range []string{"first start", "managed store", "control plane"} {
		if !strings.Contains(comment, want) {
			t.Errorf("the plugins comment does not mention %q — a reader would not learn "+
				"that editing this file after the first start does nothing", want)
		}
	}
}
