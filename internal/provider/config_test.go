package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSaveAndLoadManagedConfig(t *testing.T) {
	dir := t.TempDir()
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

