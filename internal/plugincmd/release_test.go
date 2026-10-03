package plugincmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/plugin"
)

func releaseFixture(t *testing.T) (map[string][]byte, string) {
	t.Helper()
	files := map[string][]byte{
		"plugin.json": []byte(`{"schema_version":1,"id":"torana/demo","name":"demo","version":"0.1.0","abi_version":"v1","repository":"https://github.com/torana-edge/torana-plugins","hooks":[{"name":"run_before_request"},{"name":"run_on_http_request"}],"permissions":[{"name":"env.serve_http","description":"Read plugin status"}],"failure_mode":"pass"}`),
		"plugin.wasm": {0, 'a', 's', 'm', 1, 0, 0, 0},
		"schema.json": []byte(`{"type":"object","properties":{}}`),
		"agent.json":  []byte(`{"schema_version":2,"namespace":{"title":"Demo","summary":"Demo operations","categories":["workflow"]},"operations":[{"id":"status","method":"GET","path":"/status","description":"Read status","risk":"read","idempotent":true,"model_access":"read","conversation_binding":"none","output_schema":{"type":"object"}}]}`),
	}
	dir := t.TempDir()
	var sums strings.Builder
	for _, name := range bundleFiles {
		if err := os.WriteFile(filepath.Join(dir, name), files[name], 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(files[name]), name)
	}
	digest, err := plugin.BundleDigestForDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files["SHA256SUMS"] = []byte(sums.String())
	files["BUNDLE_DIGEST"] = []byte(digest + "\n")
	files["LICENSE"] = []byte("test license")
	files["README.md"] = []byte("test release")
	return files, digest
}

func fixtureArchive(t *testing.T, files map[string][]byte, extra *tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(data)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if extra != nil {
		if err := tw.WriteHeader(extra); err != nil {
			t.Fatal(err)
		}
		if extra.Size > 0 {
			if _, err := tw.Write(make([]byte, extra.Size)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPrebuiltInstallWithoutToolchain(t *testing.T) {
	files, digest := releaseFixture(t)
	archive := fixtureArchive(t, files, nil)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/demo-0.1.0.tar.gz":
			_, _ = w.Write(archive)
		case "/demo-0.1.0.tar.gz.sha256":
			_, _ = fmt.Fprintf(w, "%x  demo-0.1.0.tar.gz\n", sha256.Sum256(archive))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	// Exercise the actual CLI dispatch with no external programs on PATH.
	t.Setenv("PATH", "")
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	dest := t.TempDir()
	var output bytes.Buffer
	if err := installPlugin([]string{server.URL + "/demo-0.1.0.tar.gz", "--dir", dest}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, err := plugin.BundleDigestForDir(filepath.Join(dest, "demo"))
	if err != nil || got != digest {
		t.Fatalf("installed digest %s, error %v; want %s", got, err, digest)
	}
	if !strings.Contains(output.String(), "NOT running yet") {
		t.Fatal("install did not preserve approval guidance")
	}
	entries, err := os.ReadDir(filepath.Join(dest, "demo"))
	if err != nil || len(entries) != 4 {
		t.Fatalf("only runtime bundle files should be installed: %v %v", entries, err)
	}
}

func TestPrebuiltRejectsUnsafeOrTamperedBundles(t *testing.T) {
	cases := []struct {
		name            string
		edit            func(map[string][]byte)
		extra           *tar.Header
		corruptChecksum bool
		wrongIdentity   bool
	}{
		{name: "wrong archive checksum", corruptChecksum: true},
		{name: "wrong bundle digest", edit: func(f map[string][]byte) { f["BUNDLE_DIGEST"] = []byte("sha256:" + strings.Repeat("0", 64)) }},
		{name: "tampered runtime file", edit: func(f map[string][]byte) { f["schema.json"] = []byte(`{"type":"string"}`) }},
		{name: "missing wasm", edit: func(f map[string][]byte) { delete(f, "plugin.wasm") }},
		{name: "missing checksum", edit: func(f map[string][]byte) { delete(f, "SHA256SUMS") }},
		{name: "extra entry", extra: &tar.Header{Name: "evil.txt", Typeflag: tar.TypeReg}},
		{name: "traversal", extra: &tar.Header{Name: "../escape", Typeflag: tar.TypeReg}},
		{name: "absolute path", extra: &tar.Header{Name: "/escape", Typeflag: tar.TypeReg}},
		{name: "symlink", extra: &tar.Header{Name: "plugin.wasm", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}},
		{name: "hardlink", extra: &tar.Header{Name: "plugin.json", Typeflag: tar.TypeLink, Linkname: "elsewhere"}},
		{name: "duplicate", extra: &tar.Header{Name: "plugin.json", Typeflag: tar.TypeReg}},
		{name: "wrong selected version", wrongIdentity: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files, _ := releaseFixture(t)
			if c.edit != nil {
				c.edit(files)
			}
			archive := fixtureArchive(t, files, c.extra)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, ".sha256") {
					hash := fmt.Sprintf("%x", sha256.Sum256(archive))
					if c.corruptChecksum {
						hash = strings.Repeat("0", 64)
					}
					_, _ = fmt.Fprintf(w, "%s  demo-0.1.0.tar.gz\n", hash)
				} else {
					_, _ = w.Write(archive)
				}
			}))
			defer server.Close()
			dest := t.TempDir()
			// An unsuccessful replacement must leave the existing plugin untouched.
			if err := os.Mkdir(filepath.Join(dest, "demo"), 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dest, "demo", "keep")
			if err := os.WriteFile(marker, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			release := pluginRelease{name: "demo", version: "0.1.0", id: "torana/demo", url: server.URL + "/demo-0.1.0.tar.gz"}
			if c.wrongIdentity {
				release.version = "9.9.9"
			}
			if err := installRelease(context.Background(), releaseClient(server.Client().Transport), release, dest, io.Discard); err == nil {
				t.Fatal("unsafe bundle was installed")
			}
			data, err := os.ReadFile(marker)
			if err != nil || string(data) != "original" {
				t.Fatal("failed install changed existing plugin")
			}
			entries, _ := os.ReadDir(dest)
			if len(entries) != 1 {
				t.Fatalf("failed install left staging files: %v", entries)
			}
		})
	}
}

func TestReleaseNetworkAndRegistry(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index":
			_, _ = io.WriteString(w, `{"schema_version":1,"plugins":[{"id":"torana/demo","name":"demo","latest":"0.1.0"}]}`)
		case "/duplicate":
			_, _ = io.WriteString(w, `{"schema_version":1,"plugins":[{"id":"torana/demo","name":"demo","latest":"0.1.0"},{"id":"torana/demo","name":"demo","latest":"0.1.0"}]}`)
		case "/redirect":
			http.Redirect(w, r, "http://127.0.0.1:1/no", http.StatusFound)
		case "/large":
			_, _ = io.WriteString(w, strings.Repeat("x", 32))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := releaseClient(server.Client().Transport)
	ctx := context.Background()
	for _, arg := range []string{"demo", "demo@0.2.0"} {
		r, err := resolveRelease(ctx, client, server.URL+"/index", arg)
		if err != nil || r.name != "demo" || !strings.HasPrefix(r.url, "https://github.com/torana-edge/torana-plugins/releases/download/demo%2Fv") {
			t.Fatalf("resolution %v %v", r, err)
		}
		if arg == "demo@0.2.0" && r.version != "0.2.0" {
			t.Fatal("version pin ignored")
		}
	}
	for _, arg := range []string{"unknown", "demo@bad", "http://example.test/demo-0.1.0.tar.gz", "https://user:pass@example.test/demo-0.1.0.tar.gz", "https://example.test/demo-bad.tar.gz"} {
		if _, err := resolveRelease(ctx, client, server.URL+"/index", arg); err == nil {
			t.Fatalf("accepted %s", arg)
		}
	}
	if _, err := resolveRelease(ctx, client, server.URL+"/duplicate", "demo"); err == nil {
		t.Fatal("duplicate registry identity accepted")
	}
	for _, p := range []string{"/redirect", "/large", "/missing"} {
		if _, err := downloadRelease(ctx, client, server.URL+p, 16); err == nil {
			t.Fatalf("accepted %s", p)
		}
	}
	for _, sum := range []string{strings.Repeat("0", 64) + "  other.tar.gz", "bad  demo.tar.gz", strings.Repeat("0", 64) + "  demo.tar.gz\n" + strings.Repeat("0", 64) + "  demo.tar.gz"} {
		if _, err := archiveChecksum([]byte(sum), "demo.tar.gz"); err == nil {
			t.Fatal("ambiguous or incorrect checksum accepted")
		}
	}
}

func TestReleaseArchiveChecksTrailer(t *testing.T) {
	files, _ := releaseFixture(t)
	archive := fixtureArchive(t, files, nil)
	for _, data := range [][]byte{archive[:len(archive)-4], append(append([]byte{}, archive...), archive...)} {
		if _, err := unpackRelease(data, t.TempDir()); err == nil {
			t.Fatal("truncated or concatenated gzip accepted")
		}
	}
}
