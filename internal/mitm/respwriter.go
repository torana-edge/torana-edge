package mitm

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// connResponseWriter adapts a raw net.Conn into an http.ResponseWriter so the
// Torana handler can serve a decrypted request directly onto the TLS
// connection. Responses are framed with Connection: close — the body ends at
// EOF, which frames both SSE streams and JSON bodies without chunking.
type connResponseWriter struct {
	conn    net.Conn
	header  http.Header
	written bool
	status  int
}

func newConnResponseWriter(conn net.Conn) *connResponseWriter {
	return &connResponseWriter{conn: conn, header: http.Header{}, status: http.StatusOK}
}

func (w *connResponseWriter) Header() http.Header { return w.header }

func (w *connResponseWriter) WriteHeader(status int) {
	if w.written {
		return
	}
	w.written = true
	w.status = status

	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	for k, vs := range w.header {
		switch strings.ToLower(k) {
		// We stream and close; upstream framing headers no longer apply.
		case "content-length", "transfer-encoding", "content-encoding", "connection":
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("Connection: close\r\n\r\n")
	_, _ = w.conn.Write([]byte(b.String()))
}

func (w *connResponseWriter) Write(p []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	return w.conn.Write(p)
}

// Flush satisfies http.Flusher; TLS conn writes already flush to the network,
// so this is a no-op that merely signals streaming support to the handler.
func (w *connResponseWriter) Flush() {}

// Close ensures the status line is emitted even for an empty response.
func (w *connResponseWriter) Close() {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
}
