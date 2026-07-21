package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestControlPlaneSPADashboard(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	provCfg := provider.DefaultConfig()
	provCfg.Port = 8080

	cfg := Config{
		Port:       "8080",
		Providers:  provCfg,
		ConfigPath: configPath,
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	go func() {
		_ = srv.Serve(ln)
	}()
	defer func() {
		_ = srv.Shutdown(context.Background())
	}()

	baseURL := "http://" + ln.Addr().String()

	// 1. Test GET /_torana/ returns 200 OK with HTML and marker string
	t.Run("GET /_torana/", func(t *testing.T) {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(baseURL + "/_torana/")
		if err != nil {
			t.Fatalf("GET /_torana/: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /_torana/ status = %d, want 200", resp.StatusCode)
		}

		contentType := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(contentType, "text/html") {
			t.Errorf("GET /_torana/ Content-Type = %q, want text/html...", contentType)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading body: %v", err)
		}
		marker := "Torana Control Plane"
		if !strings.Contains(string(body), marker) {
			t.Errorf("GET /_torana/ body missing marker string %q", marker)
		}
	})

	// 2. Test GET /_torana (no slash) returns redirect (3xx) to /_torana/
	t.Run("GET /_torana redirect", func(t *testing.T) {
		noFollowClient := &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp, err := noFollowClient.Get(baseURL + "/_torana")
		if err != nil {
			t.Fatalf("GET /_torana: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			t.Errorf("GET /_torana status = %d, want 3xx redirect", resp.StatusCode)
		}

		loc := resp.Header.Get("Location")
		if loc != "/_torana/" && !strings.HasSuffix(loc, "/_torana/") {
			t.Errorf("GET /_torana Location header = %q, want /_torana/", loc)
		}
	})

	// 3. Test that specific /_torana/api/... routes are NOT shadowed
	t.Run("API routes precedence", func(t *testing.T) {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(baseURL + "/_torana/api/config")
		if err != nil {
			t.Fatalf("GET /_torana/api/config: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /_torana/api/config status = %d, want 200", resp.StatusCode)
		}

		contentType := resp.Header.Get("Content-Type")
		if contentType != "application/json" {
			t.Errorf("GET /_torana/api/config Content-Type = %q, want application/json", contentType)
		}
	})

	// 4. Test unknown path under /_torana/ returns 404
	t.Run("Unknown subpath 404", func(t *testing.T) {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(baseURL + "/_torana/nonexistent_file.js")
		if err != nil {
			t.Fatalf("GET /_torana/nonexistent_file.js: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET /_torana/nonexistent_file.js status = %d, want 404", resp.StatusCode)
		}
	})
}
