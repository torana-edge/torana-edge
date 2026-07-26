package controlplane_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/controlplane"
)

func TestHandler(t *testing.T) {
	handler := controlplane.Handler()
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	if !strings.Contains(string(body), "Torana Control Plane") {
		t.Errorf("body missing expected title string")
	}

	for _, asset := range []struct {
		path   string
		marker string
	}{
		{path: "/tokens.css", marker: "--color-accent"},
		{path: "/app.css", marker: "designed-as-app"},
	} {
		resp, err := http.Get(srv.URL + asset.path)
		if err != nil {
			t.Fatalf("GET %s: %v", asset.path, err)
		}
		assetBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatalf("reading %s: %v", asset.path, readErr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", asset.path, resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/css") {
			t.Errorf("GET %s Content-Type = %q, want text/css", asset.path, got)
		}
		if !strings.Contains(string(assetBody), asset.marker) {
			t.Errorf("GET %s body missing marker %q", asset.path, asset.marker)
		}
	}
}
