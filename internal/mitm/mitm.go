// Package mitm provides a TLS-terminating CONNECT proxy for harnesses that
// cannot be pointed at a custom base URL (notably the Antigravity CLI, whose
// stripped Go binary ignores endpoint env vars but honors HTTPS_PROXY and a
// custom CA bundle via SSL_CERT_FILE).
//
// For hosts named in the config, the proxy decrypts the connection and routes
// chat calls (…:streamGenerateContent / …:generateContent) through the Torana
// provider pipeline so plugins run; every other host and every non-chat path is
// forwarded verbatim. The generated CA's private key stays in the configured
// dir and must never be committed or added to the system trust store.
package mitm

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// Server is the TLS-terminating CONNECT proxy.
type Server struct {
	cfg      provider.MITMConfig
	ca       *CA
	torana   http.Handler // provider-routing mux (chat calls delegate here)
	passthru *http.Transport

	// mu guards listener/closed. ListenAndServe (in its own goroutine) writes
	// listener while Close, driven by live reconfiguration, may read it
	// concurrently — the two must not race.
	mu       sync.Mutex
	listener net.Listener
	closed   bool
}

// New builds a MITM server. toranaHandler is the proxy's provider mux, obtained
// from proxy.Server.Handler().
func New(cfg provider.MITMConfig, toranaHandler http.Handler) (*Server, error) {
	if cfg.Listen == "" {
		return nil, fmt.Errorf("mitm: listen address required")
	}
	if cfg.CADir == "" {
		return nil, fmt.Errorf("mitm: ca_dir required")
	}
	ca, err := LoadOrCreateCA(cfg.CADir)
	if err != nil {
		return nil, fmt.Errorf("mitm: ca: %w", err)
	}
	bundle, err := ca.WriteBundle(cfg.CADir)
	if err != nil {
		return nil, fmt.Errorf("mitm: bundle: %w", err)
	}
	log.Printf("mitm: CA ready at %s — point the client at HTTPS_PROXY=http://%s SSL_CERT_FILE=%s",
		cfg.CADir, cfg.Listen, bundle)
	return &Server{
		cfg:      cfg,
		ca:       ca,
		torana:   toranaHandler,
		passthru: &http.Transport{ForceAttemptHTTP2: false},
	}, nil
}

// ListenAndServe starts the CONNECT proxy and blocks until it stops.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("mitm: listen %s: %w", s.cfg.Listen, err)
	}
	s.mu.Lock()
	if s.closed {
		// Close() won the race to the bind. Serving now would leak a listener
		// nobody can stop, so drop it and exit cleanly instead.
		s.mu.Unlock()
		ln.Close()
		return nil
	}
	s.listener = ln
	s.mu.Unlock()
	log.Printf("mitm: CONNECT proxy on %s; intercepting %d host(s)", s.cfg.Listen, len(s.cfg.Hosts))
	srv := &http.Server{
		Handler:      http.HandlerFunc(s.handleConnect),
		ReadTimeout:  0, // CONNECT tunnels are long-lived
		WriteTimeout: 0,
	}
	return srv.Serve(ln)
}

// Close stops the proxy. It is safe to call before ListenAndServe has bound:
// the pending bind observes the closed flag and tears its own listener down.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.listener != nil {
		err := s.listener.Close()
		s.listener = nil
		return err
	}
	return nil
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "mitm: only CONNECT", http.StatusMethodNotAllowed)
		return
	}
	hostname := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		hostname = h
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "mitm: no hijack", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		return
	}

	if _, intercept := s.cfg.Hosts[hostname]; !intercept {
		go s.tunnel(client, r.Host)
		return
	}
	go s.terminate(client, hostname)
}

// tunnel splices bytes to the real upstream without touching TLS (login,
// telemetry, and any host not in the intercept map).
func (s *Server) tunnel(client net.Conn, hostport string) {
	up, err := net.DialTimeout("tcp", hostport, 15*time.Second)
	if err != nil {
		client.Close()
		return
	}
	go func() { io.Copy(up, client); up.Close() }()
	io.Copy(client, up)
	client.Close()
}

// terminate decrypts the connection and dispatches each request: chat calls go
// through the Torana pipeline, everything else is forwarded verbatim.
func (s *Server) terminate(client net.Conn, hostname string) {
	defer client.Close()
	tlsConn := tls.Server(client, &tls.Config{
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := chi.ServerName
			if name == "" {
				name = hostname
			}
			return s.ca.LeafFor(name)
		},
		NextProtos: []string{"http/1.1"},
	})
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	defer tlsConn.Close()

	br := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		keepAlive := s.dispatch(tlsConn, req, hostname)
		if !keepAlive {
			return
		}
	}
}

// dispatch handles one decrypted request. It returns true if the connection may
// be reused for another request.
func (s *Server) dispatch(conn net.Conn, req *http.Request, hostname string) bool {
	provName := s.cfg.Hosts[hostname]
	if isChatPath(req.URL.Path) && provName != "" {
		s.routeThroughTorana(conn, req, hostname, provName)
		return false // Torana writes Connection: close-style streamed responses
	}
	return s.forwardVerbatim(conn, req, hostname)
}

// routeThroughTorana rewrites the request into a /provider/<name>/… call and
// runs it through the proxy handler, streaming the response back over conn.
func (s *Server) routeThroughTorana(conn net.Conn, req *http.Request, hostname, provName string) {
	rw := newConnResponseWriter(conn)

	// Rewrite the path into the provider namespace; the resolver strips it and
	// the Director rebuilds the upstream URL from the provider config.
	origPath := req.URL.Path
	req.URL.Scheme = ""
	req.URL.Host = ""
	req.URL.Path = provider.RoutePrefix + provName + origPath
	req.RequestURI = ""
	req.Host = hostname

	s.torana.ServeHTTP(rw, req)
	rw.Close()
	log.Printf("mitm: routed %s%s via /provider/%s", hostname, origPath, provName)
}

// forwardVerbatim proxies a non-chat request to the real host unchanged.
func (s *Server) forwardVerbatim(conn net.Conn, req *http.Request, hostname string) bool {
	target := "https://" + hostname + req.URL.RequestURI()

	out, err := http.NewRequest(req.Method, target, req.Body)
	if err != nil {
		writeSimpleError(conn, 502)
		return false
	}
	out.ContentLength = req.ContentLength
	for k, vs := range req.Header {
		if strings.EqualFold(k, "Proxy-Connection") {
			continue
		}
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	out.Header.Set("Accept-Encoding", "identity")

	resp, err := s.passthru.RoundTrip(out)
	if err != nil {
		writeSimpleError(conn, 502)
		return false
	}
	defer resp.Body.Close()

	hdr := fmt.Sprintf("HTTP/1.1 %s\r\n", resp.Status)
	for k, vs := range resp.Header {
		switch strings.ToLower(k) {
		case "content-length", "transfer-encoding", "content-encoding", "connection":
			continue
		}
		for _, v := range vs {
			hdr += fmt.Sprintf("%s: %s\r\n", k, v)
		}
	}
	hdr += "Connection: close\r\n\r\n"
	if _, err := conn.Write([]byte(hdr)); err != nil {
		return false
	}
	io.Copy(conn, resp.Body)
	return false
}

func isChatPath(path string) bool {
	return strings.Contains(path, ":streamGenerateContent") || strings.Contains(path, ":generateContent")
}

func writeSimpleError(conn net.Conn, status int) {
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Length: 0\r\n\r\n", status, http.StatusText(status))
}
