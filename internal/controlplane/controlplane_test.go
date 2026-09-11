package controlplane_test

import (
	"github.com/torana-edge/torana-edge/internal/provider"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"sort"
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

	// The settings form renders six fields per provider but saves a whole
	// provider object, so it must start from the stored one or it deletes
	// everything it does not render (pricing, cache semantics). The server
	// enforces this too — see TestSettingsSaveKeepsPricing, which is the real
	// guarantee — but losing it here means every save makes a pointless
	// round-trip through the preservation path, so pin the anchor.
	for _, marker := range []string{"dataset.originalName", "tr.dataset.originalName"} {
		if !strings.Contains(string(body), marker) {
			t.Errorf("settings form lost its stored-provider anchor: missing %q", marker)
		}
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

// The Settings form offers a provider "format" dropdown built from a hardcoded
// PROVIDER_FORMATS list in the shipped UI. That is a second copy of a
// vocabulary the server owns, and it drifted exactly as a second copy does:
// after the Bedrock adapter was deleted the UI went on offering `bedrock`, so
// an operator could select a format the server then rejects — a broken
// configuration produced by following the product's own interface.
//
// The list is checked against provider.SupportedFormats rather than restated,
// so removing or adding a format cannot silently leave the UI behind.
func TestUIProviderFormatsMatchTheServer(t *testing.T) {
	raw, err := os.ReadFile("dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`const PROVIDER_FORMATS\s*=\s*\[([^\]]*)\]`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("dist/index.html no longer declares PROVIDER_FORMATS; this check cannot " +
			"see the vocabulary the UI offers, so update it rather than deleting it")
	}
	var ui []string
	for _, q := range regexp.MustCompile(`'([^']*)'`).FindAllSubmatch(m[1], -1) {
		ui = append(ui, string(q[1]))
	}
	sort.Strings(ui)

	want := provider.SupportedFormats()
	if !slices.Equal(ui, want) {
		t.Errorf("the control-plane UI offers provider formats %v, the server accepts %v.\n"+
			"An operator can select a value the server rejects, or cannot select one it accepts.",
			ui, want)
	}
}

func TestHTTPApprovalEditorPreservesMethodSubset(t *testing.T) {
	raw, err := os.ReadFile("dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, marker := range []string{
		"Array.isArray(existing.methods) ? existing.methods : (declaration.methods || [])",
		"class=\"http-binding-method\"",
		"querySelectorAll('.http-binding-method:checked')",
		"binding.methods.length === 0",
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("HTTP approval editor lost method-subset behavior: missing %q", marker)
		}
	}
	if strings.Contains(html, "methods: JSON.parse(row.dataset.methods") {
		t.Fatal("HTTP approval save still restores every manifest method instead of the operator's subset")
	}
}
