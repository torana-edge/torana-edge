package mitm

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// TestMITMRoutesChatPathThroughTorana drives a full CONNECT→TLS→chat request
// through the ingress and asserts it reaches the Torana handler with the path
// rewritten into the provider namespace, and that the streamed body flows back.
func TestMITMRoutesChatPathThroughTorana(t *testing.T) {
	dir := t.TempDir()

	var (
		mu      sync.Mutex
		gotPath string
		gotBody string
		gotHost string
	)
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath = r.URL.Path
		gotBody = string(b)
		gotHost = r.Host
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\"}]}}\n\n")
	})

	cfg := provider.MITMConfig{
		Enabled: true,
		Listen:  "127.0.0.1:0",
		CADir:   dir,
		Hosts:   map[string]string{"cloudcode-pa.googleapis.com": "antigravity"},
	}
	s, err := New(cfg, stub)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Bind an ephemeral port ourselves so we know the address.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.listener = ln
	go func() {
		srv := &http.Server{Handler: http.HandlerFunc(s.handleConnect)}
		srv.Serve(ln)
	}()
	defer s.Close()

	proxyURL, _ := url.Parse("http://" + ln.Addr().String())
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: caPool(t, dir)},
		},
	}

	resp, err := client.Post(
		"https://cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse",
		"application/json",
		strings.NewReader(`{"request":{"contents":[]}}`),
	)
	if err != nil {
		t.Fatalf("request through MITM failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), "finishReason") {
		t.Errorf("response body not streamed back: %q", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/provider/antigravity/v1internal:streamGenerateContent" {
		t.Errorf("path not rewritten into provider namespace: %q", gotPath)
	}
	if gotBody != `{"request":{"contents":[]}}` {
		t.Errorf("request body not forwarded intact: %q", gotBody)
	}
	if gotHost != "cloudcode-pa.googleapis.com" {
		t.Errorf("host not preserved: %q", gotHost)
	}
}

func TestLeafForIsValidForHost(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.LeafFor("daily-cloudcode-pa.googleapis.com")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname("daily-cloudcode-pa.googleapis.com"); err != nil {
		t.Errorf("leaf not valid for host: %v", err)
	}
	// Chain to the CA.
	roots := caPool(t, dir)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "daily-cloudcode-pa.googleapis.com"}); err != nil {
		t.Errorf("leaf does not chain to CA: %v", err)
	}
}

func caPool(t *testing.T, dir string) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	b, err := os.ReadFile(filepath.Join(dir, "ca-cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(b)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pool.AddCert(cert)
	return pool
}
