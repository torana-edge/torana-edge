package controlclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/instance"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestActiveRecordBindsRequestsAndStaleRecordsAreIgnored(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TORANA_DATA_DIR", dir)
	t.Setenv("TORANA_PORT", "8282")
	t.Setenv("TORANA_BIND", "127.0.0.1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Torana-Instance-ID"); got != "expected-instance" {
			t.Errorf("instance header = %q", got)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	path := filepath.Join(dir, "instance.json")
	if err := instance.WriteRecord(path, instance.Record{Address: srv.URL, InstanceID: "expected-instance"}); err != nil {
		t.Fatal(err)
	}
	owner, err := instance.Acquire(filepath.Join(dir, "instance.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	c, err := New("", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, _, err := c.JSON(context.Background(), http.MethodGet, BasePath+"/config", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := instance.WriteRecord(path, instance.Record{Address: "https://example.com", InstanceID: "expected-instance"}); err != nil {
		t.Fatal(err)
	}
	if bad, err := New("", time.Second); err == nil {
		bad.Close()
		t.Fatal("record bypassed loopback address boundary")
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if addr, err := DefaultAddress(); err != nil || addr != "127.0.0.1:8282" {
		t.Fatalf("stale record still used: %q, %v", addr, err)
	}
}

func TestAddressBoundary(t *testing.T) {
	for _, addr := range []string{"https://example.com", "http://192.168.1.1:8080", "http://0.0.0.0:8080", "http://[::]:8080", "http://user:secret@localhost:8080", "http://localhost/config", "http://localhost?", "http://localhost?token=x", "http://localhost#x", "ftp://localhost", "http://localhost:0", "http://localhost:65536"} {
		if c, err := New(addr, time.Second); err == nil {
			c.Close()
			t.Errorf("accepted %s", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8080", "localhost:8080", "http://[::1]:8080", "https://localhost/"} {
		c, err := New(addr, time.Second)
		if err != nil {
			t.Errorf("%s: %v", addr, err)
		} else {
			c.Close()
		}
	}
}

func TestRequestsStayLocalAndDoNotFollowRedirects(t *testing.T) {
	var leaked bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true; w.Write([]byte(`{}`)) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Torana-Local-Request") != "1" || r.Header.Get("If-Match") != `"revision"` {
			t.Error("missing local marker or revision")
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	t.Setenv("HTTP_PROXY", target.URL)
	c, err := New(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _, err = c.JSON(context.Background(), http.MethodPut, BasePath+"/config", []byte(`{}`), `"revision"`)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 307 {
		t.Fatalf("redirect error = %v", err)
	}
	if leaked {
		t.Fatal("redirect or HTTP_PROXY received request")
	}
	for _, path := range []string{"https://localhost/config", "//localhost/config", "/other", BasePath + "/../secret", BasePath + "/%2e%2e/secret", BasePath + "/config#x"} {
		if _, err := c.Open(context.Background(), http.MethodPut, path, nil, ""); err == nil {
			t.Errorf("accepted unsafe path %q", path)
		}
	}
}

func TestReadBoundedAndStructuredFailures(t *testing.T) {
	if _, err := ReadBounded(strings.NewReader(strings.Repeat("x", MaxBodyBytes+1))); err == nil {
		t.Fatal("oversize response accepted")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
		io.WriteString(w, `{"error":{"code":"stale_revision","message":"stale"}}`)
	}))
	defer srv.Close()
	c, err := New(srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _, err = c.JSON(context.Background(), "PUT", BasePath+"/plugins", []byte(`{}`), `"old"`)
	var e *APIError
	if !errors.As(err, &e) || e.Code != "stale_revision" || !strings.Contains(e.Message, "fresh snapshot") {
		t.Fatalf("error = %v", err)
	}
}

func TestDefaultAddressIsReadOnlyAndUsesConfiguredPort(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TORANA_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("TORANA_CONFIG", filepath.Join(dir, "seed.json"))
	t.Setenv("TORANA_PORT", "")
	t.Setenv("TORANA_BIND", "")
	if got, err := DefaultAddress(); err != nil || got != "127.0.0.1:8080" {
		t.Fatalf("default = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "data")); !os.IsNotExist(err) {
		t.Fatalf("read created a managed store: %v", err)
	}
	cfg := provider.DefaultConfig()
	cfg.Port = 8181
	if err := provider.Save(filepath.Join(dir, "seed.json"), cfg); err != nil {
		t.Fatal(err)
	}
	if got, err := DefaultAddress(); err != nil || got != "127.0.0.1:8181" {
		t.Fatalf("seed = %q, %v", got, err)
	}
	cfg.Port = 8282
	if err := provider.Save(filepath.Join(dir, "data", "config.json"), cfg); err != nil {
		t.Fatal(err)
	}
	if got, err := DefaultAddress(); err != nil || got != "127.0.0.1:8282" {
		t.Fatalf("managed = %q, %v", got, err)
	}
	t.Setenv("TORANA_PORT", "8383")
	t.Setenv("TORANA_BIND", "::")
	if got, err := DefaultAddress(); err != nil || got != "[::1]:8383" {
		t.Fatalf("override = %q, %v", got, err)
	}
	t.Setenv("TORANA_PORT", "808O")
	if _, err := DefaultAddress(); err == nil {
		t.Fatal("ignored invalid port")
	}
}
