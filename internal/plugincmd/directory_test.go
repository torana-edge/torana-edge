package plugincmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/torana-edge/torana-edge/internal/instance"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestPluginDirectoryResolution(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("TORANA_DATA_DIR", root)
	t.Setenv("TORANA_PLUGINS_DIR", "")
	t.Setenv("TORANA_CONFIG", filepath.Join(root, "seed.json"))
	check := func(explicit, want string) {
		t.Helper()
		got, err := pluginsDir(explicit)
		if err != nil || got != want {
			t.Fatalf("directory=%q err=%v want=%q", got, err, want)
		}
	}
	check("", filepath.Join(root, "plugins"))
	if _, err := os.Stat(filepath.Join(root, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("read-only directory lookup materialized config: %v", err)
	}
	cfg := provider.DefaultConfig()
	cfg.Plugins.Dir = filepath.Join(root, "seed-plugins")
	if err := provider.Save(os.Getenv("TORANA_CONFIG"), cfg); err != nil {
		t.Fatal(err)
	}
	check("", cfg.Plugins.Dir)
	cfg.Plugins.Dir = filepath.Join(root, "managed-plugins")
	if err := provider.Save(filepath.Join(root, "config.json"), cfg); err != nil {
		t.Fatal(err)
	}
	check("", cfg.Plugins.Dir)
	t.Setenv("TORANA_PLUGINS_DIR", "env-plugins")
	check("", "env-plugins")
	check("explicit-plugins", "explicit-plugins")
	t.Setenv("TORANA_PLUGINS_DIR", "")
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pluginsDir(""); err == nil {
		t.Fatal("invalid config silently fell back")
	}
	check("explicit-plugins", "explicit-plugins")
}

func TestPluginDirectoryFollowsRunningInstanceFromAnotherDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TORANA_DATA_DIR", root)
	t.Setenv("TORANA_PLUGINS_DIR", "")
	t.Chdir(t.TempDir())
	want := filepath.Join(root, "actual-running-plugins")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_torana/api/v1/system" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"plugin_directory": want})
	}))
	defer server.Close()
	owner, err := instance.Acquire(filepath.Join(root, "instance.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := instance.WriteRecord(filepath.Join(root, "instance.json"), instance.Record{Address: server.URL, InstanceID: "validation"}); err != nil {
		t.Fatal(err)
	}
	got, err := pluginsDir("")
	if err != nil || got != want {
		t.Fatalf("directory=%q err=%v want=%q", got, err, want)
	}
}

func TestPluginDirectoryIgnoresUnrelatedWorkingDirectoryConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	t.Setenv("TORANA_PLUGINS_DIR", "")
	t.Setenv("TORANA_CONFIG", "")
	if err := os.WriteFile("config.json", []byte(`{"project":"not-torana"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := pluginsDir("")
	want := filepath.Join(os.Getenv("TORANA_DATA_DIR"), "plugins")
	if err != nil || got != want {
		t.Fatalf("directory=%q err=%v want=%q", got, err, want)
	}
	t.Setenv("TORANA_CONFIG", filepath.Join(".", "config.json"))
	if _, err := pluginsDir(""); err == nil {
		t.Fatal("explicitly selected invalid Torana seed was ignored")
	}
}
