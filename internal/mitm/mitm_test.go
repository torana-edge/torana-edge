package mitm

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/fileperm"
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
		Hosts:   map[string]string{"CloudCode-PA.GoogleAPIs.COM.": "antigravity"},
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

func TestMITMRejectsMismatchedSNI(t *testing.T) {
	dir := t.TempDir()
	s, err := New(provider.MITMConfig{
		Enabled: true,
		Listen:  "127.0.0.1:0",
		CADir:   dir,
		Hosts:   map[string]string{"api.example.com": "provider"},
	}, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.listener = ln
	go func() {
		_ = (&http.Server{Handler: http.HandlerFunc(s.handleConnect)}).Serve(ln)
	}()
	defer s.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprint(conn, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d", resp.StatusCode)
	}
	tlsConn := tls.Client(conn, &tls.Config{ //nolint:gosec // mismatch rejection is the subject; trust is irrelevant
		InsecureSkipVerify: true,
		ServerName:         "other.example.com",
	})
	if err := tlsConn.Handshake(); err == nil {
		t.Fatal("TLS handshake with an SNI outside the CONNECT authority succeeded")
	}
}

func TestNewRejectsNonLoopbackListener(t *testing.T) {
	_, err := New(provider.MITMConfig{
		Enabled: true,
		Listen:  "0.0.0.0:8099",
		CADir:   t.TempDir(),
		Hosts:   map[string]string{"api.example.com": "provider"},
	}, http.NotFoundHandler())
	if err == nil || !strings.Contains(err.Error(), "literal loopback") {
		t.Fatalf("New error = %v", err)
	}
}

func TestRemoveHopByHopHeaders(t *testing.T) {
	header := http.Header{
		"Authorization":       {"Bearer provider-token"},
		"Connection":          {"keep-alive, X-Connection-Secret"},
		"Keep-Alive":          {"timeout=5"},
		"Proxy-Authorization": {"Basic proxy-secret"},
		"Proxy-Connection":    {"keep-alive"},
		"Te":                  {"trailers"},
		"Trailer":             {"X-Trailer"},
		"Transfer-Encoding":   {"chunked"},
		"Upgrade":             {"websocket"},
		"X-Connection-Secret": {"secret"},
	}
	removeHopByHopHeaders(header)
	if got := header.Get("Authorization"); got != "Bearer provider-token" {
		t.Fatalf("end-to-end authorization = %q", got)
	}
	for _, name := range []string{
		"Connection", "Keep-Alive", "Proxy-Authorization", "Proxy-Connection",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade", "X-Connection-Secret",
	} {
		if values, ok := header[name]; ok || len(values) != 0 {
			t.Errorf("hop-by-hop header %s survived: %v", name, values)
		}
	}
}

func TestDispatchStripsHopByHopHeadersBeforeTorana(t *testing.T) {
	var saw http.Header
	s := &Server{
		cfg: provider.MITMConfig{Hosts: map[string]string{"api.example.com": "provider"}},
		torana: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			saw = r.Header.Clone()
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	req := httptest.NewRequest(http.MethodPost, "https://api.example.com/v1:generateContent", nil)
	req.Header.Set("Authorization", "Bearer provider-token")
	req.Header.Set("Connection", "X-Connection-Secret")
	req.Header.Set("X-Connection-Secret", "secret")
	req.Header.Set("Proxy-Authorization", "Basic proxy-secret")
	owned, peer := net.Pipe()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, peer)
		close(done)
	}()
	s.dispatch(context.Background(), owned, req, "api.example.com")
	_ = owned.Close()
	_ = peer.Close()
	<-done
	if got := saw.Get("Authorization"); got != "Bearer provider-token" {
		t.Fatalf("end-to-end authorization = %q", got)
	}
	for _, name := range []string{"Connection", "Proxy-Authorization", "X-Connection-Secret"} {
		if got := saw.Values(name); len(got) != 0 {
			t.Errorf("Torana handler saw hop-by-hop header %s: %v", name, got)
		}
	}
}

func TestCloseOwnsHijackedConnections(t *testing.T) {
	s, err := New(provider.MITMConfig{
		Enabled: true,
		Listen:  "127.0.0.1:8099",
		CADir:   t.TempDir(),
		Hosts:   map[string]string{"api.example.com": "provider"},
	}, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	owned, peer := net.Pipe()
	defer peer.Close()
	if !s.track(owned) {
		t.Fatal("fresh server refused a connection")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("peer remained readable after server Close")
	}
	late, latePeer := net.Pipe()
	defer late.Close()
	defer latePeer.Close()
	if s.track(late) {
		t.Fatal("closed server accepted a late connection")
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
	reloaded, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("reload valid CA: %v", err)
	}
	if !reloaded.cert.Equal(ca.cert) {
		t.Fatal("reloaded CA certificate changed")
	}
}

func TestCALoadFailsClosed(t *testing.T) {
	t.Run("private key is not owner-only", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := LoadOrCreateCA(dir); err != nil {
			t.Fatal(err)
		}
		keyPath := filepath.Join(dir, "ca-key.pem")
		key, err := os.ReadFile(keyPath)
		if err != nil {
			t.Fatal(err)
		}
		// Widen it the same way on both platforms. os.Chmod would only mean
		// something on Unix; rewriting the file with no explicit protection
		// leaves 0644 there and, on Windows, a DACL merely INHERITED from the
		// directory — which a later change to the parent can widen without
		// touching this file, and which fileperm therefore refuses.
		if err := os.Remove(keyPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyPath, key, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateCA(dir); err == nil || !strings.Contains(err.Error(), "private key") {
			t.Fatalf("LoadOrCreateCA accepted a CA private key that is not owner-only: %v", err)
		}
	})

	t.Run("incomplete pair", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "ca-cert.pem"), []byte("not a cert"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateCA(dir); err == nil || !strings.Contains(err.Error(), "incomplete MITM CA") {
			t.Fatalf("LoadOrCreateCA error = %v", err)
		}
	})

	t.Run("malformed pair", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := LoadOrCreateCA(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ca-cert.pem"), []byte("not a PEM block"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateCA(dir); err == nil || !strings.Contains(err.Error(), "exactly one CERTIFICATE") {
			t.Fatalf("LoadOrCreateCA error = %v", err)
		}
	})

	t.Run("mismatched pair", func(t *testing.T) {
		first := t.TempDir()
		second := t.TempDir()
		if _, err := LoadOrCreateCA(first); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateCA(second); err != nil {
			t.Fatal(err)
		}
		otherKey, err := os.ReadFile(filepath.Join(second, "ca-key.pem"))
		if err != nil {
			t.Fatal(err)
		}
		// 0600 alone does not make a file owner-only on Windows, where a
		// newly written file inherits the directory's ACL. Without the
		// explicit restriction this fixture trips the key-permission check
		// and never reaches the mismatch it exists to test.
		firstKey := filepath.Join(first, "ca-key.pem")
		if err := os.WriteFile(firstKey, otherKey, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := fileperm.Restrict(firstKey); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateCA(first); err == nil || !strings.Contains(err.Error(), "do not match") {
			t.Fatalf("LoadOrCreateCA error = %v", err)
		}
	})

	t.Run("symlinked key", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := LoadOrCreateCA(dir); err != nil {
			t.Fatal(err)
		}
		keyPath := filepath.Join(dir, "ca-key.pem")
		realPath := filepath.Join(dir, "real-key.pem")
		if err := os.Rename(keyPath, realPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realPath, keyPath); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateCA(dir); err == nil || !strings.Contains(err.Error(), "must not be symlinks") {
			t.Fatalf("LoadOrCreateCA error = %v", err)
		}
	})

	t.Run("symlinked directory", func(t *testing.T) {
		parent := t.TempDir()
		realDir := filepath.Join(parent, "real")
		if err := os.Mkdir(realDir, 0o700); err != nil {
			t.Fatal(err)
		}
		linkDir := filepath.Join(parent, "link")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateCA(linkDir); err == nil || !strings.Contains(err.Error(), "directory must not be a symlink") {
			t.Fatalf("LoadOrCreateCA error = %v", err)
		}
	})

	t.Run("CA directory permissions", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateCA(dir); err != nil {
			t.Fatal(err)
		}
		// The invariant is that nobody else can reach the CA material, which
		// is mode bits on Unix and a DACL on Windows — where Perm() reports
		// 0777 for any directory and asserting 0700 proves nothing.
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := fileperm.Verify(dir, info); err != nil {
			t.Errorf("CA directory is not owner-only: %v", err)
		}
	})
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

// TestClientDisconnectCancelsUpstream is the property the previous fix did not
// actually have: terminate's deferred cancel is unreachable while terminate is
// blocked inside dispatch, which is the whole interval a disconnected harness
// should stop paying for. The upstream here waits on req.Context().Done() and
// nothing else, so the test can only pass if the closed client connection —
// not the end of the request — is what cancels it.
func TestClientDisconnectCancelsUpstream(t *testing.T) {
	dir := t.TempDir()

	reached := make(chan struct{})
	cancelled := make(chan struct{})
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(reached)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(10 * time.Second):
			// Falls through without closing cancelled; the assertion below
			// reports the failure with a useful message.
		}
	})

	s, err := New(provider.MITMConfig{
		Enabled: true,
		Listen:  "127.0.0.1:0",
		CADir:   dir,
		Hosts:   map[string]string{"cloudcode-pa.googleapis.com": "antigravity"},
	}, stub)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.listener = ln
	go func() {
		srv := &http.Server{Handler: http.HandlerFunc(s.handleConnect)}
		_ = srv.Serve(ln)
	}()
	defer s.Close()

	// A raw connection, so the client side can be closed mid-request. An
	// http.Client would hide the connection and only close it once the
	// response completed — which is exactly the case that already worked.
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := fmt.Fprintf(raw, "CONNECT cloudcode-pa.googleapis.com:443 HTTP/1.1\r\nHost: cloudcode-pa.googleapis.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(raw)
	connectResp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("CONNECT: %v", err)
	}
	if connectResp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d", connectResp.StatusCode)
	}

	tlsConn := tls.Client(raw, &tls.Config{
		ServerName: "cloudcode-pa.googleapis.com",
		RootCAs:    caPool(t, dir),
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	body := `{"request":{"contents":[]}}`
	if _, err := fmt.Fprintf(tlsConn,
		"POST /v1internal:streamGenerateContent?alt=sse HTTP/1.1\r\nHost: cloudcode-pa.googleapis.com\r\n"+
			"Content-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		t.Fatal(err)
	}

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the Torana handler")
	}

	// The handler is now blocked with the request in flight. Drop the client.
	_ = raw.Close()

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream was still running after the client disconnected: " +
			"the provider keeps generating, and billing, for a harness that has quit")
	}
}

// TestHealthyStreamOutlivesTheIdleBound: the idle bound exists to reap a
// connection that says nothing, not to cap one that is working. A single
// absolute SetDeadline covered reads AND writes for the whole connection and
// was never refreshed by progress, so a stream still delivering tokens when it
// elapsed was killed mid-response — the failure mode is a truncated model
// answer, which looks like a provider problem.
//
// idleTimeout is shortened here so the property is provable in a second rather
// than ten minutes.
func TestHealthyStreamOutlivesTheIdleBound(t *testing.T) {
	dir := t.TempDir()

	const chunks = 10
	const gap = 40 * time.Millisecond
	var (
		mu       sync.Mutex
		ctxErrAt = -1 // chunk index at which the request context was cancelled
	)
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for i := range chunks {
			if r.Context().Err() != nil {
				mu.Lock()
				if ctxErrAt < 0 {
					ctxErrAt = i
				}
				mu.Unlock()
				return
			}
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(gap)
		}
	})

	s, err := New(provider.MITMConfig{
		Enabled: true,
		Listen:  "127.0.0.1:0",
		CADir:   dir,
		Hosts:   map[string]string{"cloudcode-pa.googleapis.com": "antigravity"},
	}, stub)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Far shorter than the stream takes to finish. Under the old absolute
	// deadline the connection dies around chunk 2.
	s.idleTimeout = 3 * gap

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.listener = ln
	go func() {
		srv := &http.Server{Handler: http.HandlerFunc(s.handleConnect)}
		_ = srv.Serve(ln)
	}()
	defer s.Close()

	proxyURL, _ := url.Parse("http://" + ln.Addr().String())
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: caPool(t, dir)},
		},
	}
	resp, err := client.Post(
		"https://cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse",
		"application/json", strings.NewReader(`{"request":{"contents":[]}}`))
	if err != nil {
		t.Fatalf("request through MITM failed: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("stream cut off after %d bytes: %v", len(body), err)
	}
	mu.Lock()
	cancelledAt := ctxErrAt
	mu.Unlock()
	if cancelledAt >= 0 {
		t.Errorf("the request context was cancelled at chunk %d of %d while the client was "+
			"still connected and reading — the idle bound is being applied to a healthy "+
			"request instead of to idleness", cancelledAt, chunks)
	}
	for i := range chunks {
		if want := fmt.Sprintf("data: chunk-%d\n", i); !strings.Contains(string(body), want) {
			t.Fatalf("stream truncated at chunk %d — the idle bound is capping a healthy "+
				"response, not idleness\n  got: %q", i, body)
		}
	}
}
