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
	"bytes"
	"context"
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

// idleTimeout bounds how long a decrypted connection may sit before its
// request arrives. It is a READ bound on silence, not a cap on how long the
// request may then take: a model that streams for half an hour is healthy, and
// the connection is alive the whole time.
const idleTimeout = 10 * time.Minute

// handshakeTimeout bounds the TLS handshake itself.
const handshakeTimeout = 15 * time.Second

// Server is the TLS-terminating CONNECT proxy.
type Server struct {
	cfg      provider.MITMConfig
	ca       *CA
	torana   http.Handler // provider-routing mux (chat calls delegate here)
	passthru *http.Transport

	// mu guards listener/closed. ListenAndServe (in its own goroutine) writes
	// listener while Close, driven by live reconfiguration, may read it
	// concurrently — the two must not race.
	// idleTimeout is the pre-request read bound, a field so a test can prove
	// it does NOT cap a healthy stream without waiting ten minutes for it.
	idleTimeout time.Duration

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	closed   bool
}

// New builds a MITM server. toranaHandler is the proxy's provider mux, obtained
// from proxy.Server.Handler().
func New(cfg provider.MITMConfig, toranaHandler http.Handler) (*Server, error) {
	if err := cfg.ValidateIngress(); err != nil {
		return nil, fmt.Errorf("mitm: %w", err)
	}
	hosts := make(map[string]string, len(cfg.Hosts))
	for raw, providerName := range cfg.Hosts {
		hostname, err := provider.CanonicalMITMHostname(raw)
		if err != nil {
			return nil, fmt.Errorf("mitm: %w", err)
		}
		hosts[hostname] = providerName
	}
	cfg.Hosts = hosts
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
		cfg:    cfg,
		ca:     ca,
		torana: toranaHandler,
		passthru: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     false,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
		idleTimeout: idleTimeout,
		conns:       make(map[net.Conn]struct{}),
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
	var firstErr error
	if s.listener != nil {
		firstErr = s.listener.Close()
		s.listener = nil
	}
	for conn := range s.conns {
		if err := conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(s.conns, conn)
	}
	return firstErr
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
	canonical, err := provider.CanonicalMITMHostname(hostname)
	if err != nil {
		http.Error(w, "mitm: invalid CONNECT host", http.StatusBadRequest)
		return
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
	if !s.track(client) {
		_ = client.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		s.untrack(client)
		_ = client.Close()
		return
	}

	if _, intercept := s.cfg.Hosts[canonical]; !intercept {
		go func() {
			defer s.untrack(client)
			s.tunnel(client, r.Host)
		}()
		return
	}
	go func() {
		defer s.untrack(client)
		s.terminate(client, canonical)
	}()
}

func (s *Server) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

func (s *Server) untrack(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

// tunnel splices bytes to the real upstream without touching TLS (login,
// telemetry, and any host not in the intercept map).
//
// The destination is whatever the CONNECT names, which makes this listener a
// forward proxy for the local machine. That is inherent to being a CONNECT
// proxy and the listener is loopback-only, but it is a real surface: see
// docs/GEMINI_ANTIGRAVITY.md, which now says so rather than leaving a reader
// to infer it.
func (s *Server) tunnel(client net.Conn, hostport string) {
	up, err := net.DialTimeout("tcp", hostport, 15*time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
	go func() {
		_, _ = io.Copy(up, client)
		_ = up.Close()
	}()
	_, _ = io.Copy(client, up)
	_ = client.Close()
}

// terminate decrypts the connection and dispatches each request: chat calls go
// through the Torana pipeline, everything else is forwarded verbatim.
func (s *Server) terminate(client net.Conn, hostname string) {
	defer func() { _ = client.Close() }()
	if err := client.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return
	}
	tlsConn := tls.Server(client, &tls.Config{
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := chi.ServerName
			if name == "" {
				name = hostname
			}
			canonical, err := provider.CanonicalMITMHostname(name)
			if err != nil {
				return nil, err
			}
			if canonical != hostname {
				return nil, fmt.Errorf("mitm: TLS server name %q does not match CONNECT host %q", name, hostname)
			}
			return s.ca.LeafFor(canonical)
		},
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	})
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	defer func() { _ = tlsConn.Close() }()

	// An idle bound on the REQUEST ARRIVING, not "no deadline at all" and not
	// a cap on serving it. One absolute SetDeadline covered reads AND writes
	// for the whole connection, so a healthy stream still delivering tokens
	// ten minutes in was killed mid-response; writes carry their own per-write
	// bound instead (see writeTimeout), refreshed by actual progress.
	if err := client.SetWriteDeadline(time.Time{}); err != nil {
		return
	}
	if err := client.SetReadDeadline(time.Now().Add(s.idleTimeout)); err != nil {
		return
	}

	// Tie a context to this connection so that when the client goes away the
	// request Torana is running on its behalf is cancelled too. Requests read
	// off this connection carry context.Background() otherwise: the provider
	// keeps generating — and billing — after the harness has quit.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	br := bufio.NewReader(tlsConn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	// The request is in hand, so the idle bound has done its job. Leaving it
	// armed would turn it into a total request lifetime.
	if err := client.SetReadDeadline(time.Time{}); err != nil {
		return
	}
	watchPeerGone(ctx, cancel, tlsConn, req)
	s.dispatch(ctx, tlsConn, req, hostname)
}

// requestBufferLimit caps how much of a request body watchPeerGone will buffer
// in order to free the socket. Chat requests sit far below it; anything larger
// keeps streaming off the connection and simply forgoes the watcher rather
// than being truncated.
const requestBufferLimit = 32 << 20

// watchPeerGone cancels ctx when the client goes away while Torana is still
// serving its request.
//
// terminate's deferred cancel cannot do this on its own: terminate is blocked
// inside dispatch until the upstream finishes, and that is exactly the window
// that matters — a harness killed mid-generation should stop the provider
// generating, and billing, not have the cancel fire once the tokens are
// already paid for. Only an independent reader notices the peer during it.
//
// The body is buffered first so the watcher cannot race the pipeline for its
// bytes. Waiting for the body to be CONSUMED instead does not work: a handler
// that never reads it — an early rejection, a plugin veto — would leave the
// watcher disarmed for the whole request, which is the case that matters most.
//
// Once armed, anything arriving would belong to a pipelined request this
// server does not serve, so bytes are discarded and only a read ERROR counts
// as the peer being gone. The goroutine exits when the connection closes,
// which terminate's defers do on every path.
func watchPeerGone(ctx context.Context, cancel context.CancelFunc, conn net.Conn, req *http.Request) {
	if req.Body != nil && req.Body != http.NoBody {
		buf, err := io.ReadAll(io.LimitReader(req.Body, requestBufferLimit+1))
		if err != nil || len(buf) > requestBufferLimit {
			// Splice back what was consumed and let the body stream from the
			// socket as before. The request still works; it just runs without
			// a disconnect watcher, exactly as every request used to.
			req.Body = spliced{Reader: io.MultiReader(bytes.NewReader(buf), req.Body), Closer: req.Body}
			return
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(buf))
		req.ContentLength = int64(len(buf))
	}
	go func() {
		buf := make([]byte, 512)
		for {
			if _, err := conn.Read(buf); err != nil {
				cancel()
				return
			}
		}
	}()
}

// spliced re-presents a partially consumed body as a single ReadCloser.
type spliced struct {
	io.Reader
	io.Closer
}

// dispatch handles the one decrypted request this tunnel carries: chat calls
// go through the Torana pipeline, everything else is forwarded verbatim.
//
// One request per tunnel, by construction. Both paths frame their response
// with Connection: close, the caller closes the connection when dispatch
// returns, and watchPeerGone is reading the socket by then — a second request
// could not be parsed off it anyway. What stood here was a read loop and a
// "the connection may be reused" return value describing keep-alive that
// neither path implemented and both returned false for.
func (s *Server) dispatch(ctx context.Context, conn net.Conn, req *http.Request, hostname string) {
	removeHopByHopHeaders(req.Header)
	req.TransferEncoding = nil
	req.Trailer = nil
	provName := s.cfg.Hosts[hostname]
	if isChatPath(req.URL.Path) && provName != "" {
		s.routeThroughTorana(ctx, conn, req, hostname, provName)
		return
	}
	s.forwardVerbatim(ctx, conn, req, hostname)
}

// routeThroughTorana rewrites the request into a /provider/<name>/… call and
// runs it through the proxy handler, streaming the response back over conn.
func (s *Server) routeThroughTorana(ctx context.Context, conn net.Conn, req *http.Request, hostname, provName string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rw := newConnResponseWriter(conn, cancel)
	req = req.WithContext(ctx)

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
func (s *Server) forwardVerbatim(ctx context.Context, conn net.Conn, req *http.Request, hostname string) {
	target := "https://" + hostname + req.URL.RequestURI()

	out, err := http.NewRequestWithContext(ctx, req.Method, target, req.Body)
	if err != nil {
		writeSimpleError(conn, 502)
		return
	}
	out.ContentLength = req.ContentLength
	for k, vs := range req.Header {
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	out.Header.Set("Accept-Encoding", "identity")

	resp, err := s.passthru.RoundTrip(out)
	if err != nil {
		writeSimpleError(conn, 502)
		return
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
	// Per-write deadlines, refreshed by progress: a client that has stopped
	// reading is cut off without capping how long a healthy transfer may run.
	cw := deadlineWriter{conn: conn}
	if _, err := cw.Write([]byte(hdr)); err != nil {
		return
	}
	_, _ = io.Copy(cw, resp.Body)
}

func removeHopByHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			if name := strings.TrimSpace(token); name != "" {
				header.Del(name)
			}
		}
	}
	for _, name := range []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Proxy-Connection",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		header.Del(name)
	}
}

func isChatPath(path string) bool {
	return strings.Contains(path, ":streamGenerateContent") || strings.Contains(path, ":generateContent")
}

func writeSimpleError(conn net.Conn, status int) {
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Length: 0\r\n\r\n", status, http.StatusText(status))
}
