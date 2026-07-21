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
