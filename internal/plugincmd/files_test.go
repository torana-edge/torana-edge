package plugincmd

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPluginFilePathPrintsOnlyServerResolvedPath(t *testing.T) {
	want := filepath.Join(t.TempDir(), "plugin-data", "digest", "usage.jsonl")
	var gotPlugin, gotLogical string
	var stdout bytes.Buffer
	err := pluginFileWithPathResolver([]string{"path", "usage_logger", "usage.jsonl"}, &stdout, func(plugin, logical string) (string, error) {
		gotPlugin, gotLogical = plugin, logical
		return want, nil
	})
	if err != nil {
		t.Fatalf("pluginFile: %v", err)
	}
	if gotPlugin != "usage_logger" || gotLogical != "usage.jsonl" {
		t.Fatalf("resolver args = %q, %q", gotPlugin, gotLogical)
	}
	if got := stdout.String(); got != want+"\n" {
		t.Fatalf("stdout = %q, want only %q", got, want+"\n")
	}
	if !filepath.IsAbs(strings.TrimSpace(stdout.String())) {
		t.Fatalf("path is not absolute: %q", stdout.String())
	}
}

func TestPluginFilePathRejectsUnsafeAndMalformedInputs(t *testing.T) {
	for _, args := range [][]string{
		{"path", "usage_logger"},
		{"path", "usage_logger", "usage.jsonl", "extra"},
	} {
		var stdout bytes.Buffer
		if err := pluginFileWithPathResolver(args, &stdout, func(_, _ string) (string, error) {
			t.Fatal("resolver called for malformed command")
			return "", nil
		}); err == nil {
			t.Errorf("pluginFile(%q) succeeded with stdout %q", args, stdout.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("pluginFile(%q) wrote stdout on failure: %q", args, stdout.String())
		}
	}
	var stdout bytes.Buffer
	err := pluginFileWithPathResolver([]string{"path", "usage_logger", "usage.jsonl"}, &stdout, func(_, _ string) (string, error) {
		return "", errors.New("refused")
	})
	if err == nil || stdout.Len() != 0 {
		t.Fatalf("resolver failure = %v, stdout %q", err, stdout.String())
	}
}

func TestPluginFilePathAtUsesRunningControlPlane(t *testing.T) {
	want := filepath.Join(t.TempDir(), "plugin-data", "digest", "usage.jsonl")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_torana/api/v1/plugin-files/path" || r.URL.Query().Get("plugin") != "usage logger" || r.URL.Query().Get("logical") != "nested/usage.jsonl" {
			t.Errorf("request = %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"path":`+strconv.Quote(want)+`}`)
	}))
	defer server.Close()
	got, err := pluginFilePathAt(server.Client(), server.URL, "usage logger", "nested/usage.jsonl")
	if err != nil || got != want {
		t.Fatalf("path = %q, %v; want %q", got, err, want)
	}
}

func TestPluginFilePathAtRejectsBadServerResponses(t *testing.T) {
	for _, response := range []struct {
		status int
		body   string
	}{
		{http.StatusBadRequest, "invalid plugin file path\n"},
		{http.StatusOK, `{}`},
		{http.StatusOK, `{"path":"relative"}`},
		{http.StatusOK, `{"path":"/tmp/x","extra":true}`},
		{http.StatusOK, `{"path":"/tmp/x"}{}`},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(response.status)
			_, _ = io.WriteString(w, response.body)
		}))
		_, err := pluginFilePathAt(server.Client(), server.URL, "usage_logger", "usage.jsonl")
		server.Close()
		if err == nil {
			t.Errorf("response %d %q was accepted", response.status, response.body)
		}
	}
}
