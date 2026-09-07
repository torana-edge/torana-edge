package mitm

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// writeTimeout bounds a SINGLE write to the client and is refreshed on every
// write, so a stalled reader is cut off while a healthy stream runs as long as
// it needs. An absolute connection deadline could not tell those apart: it
// killed a stream that had been delivering tokens continuously.
const writeTimeout = 30 * time.Second

// deadlineWriter arms writeTimeout before each write to conn.
type deadlineWriter struct{ conn net.Conn }

func (d deadlineWriter) Write(p []byte) (int, error) {
	_ = d.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return d.conn.Write(p)
}

// connResponseWriter adapts a raw net.Conn into an http.ResponseWriter so the
// Torana handler can serve a decrypted request directly onto the TLS
// connection. Responses are framed with Connection: close — the body ends at
// EOF, which frames both SSE streams and JSON bodies without chunking.
type connResponseWriter struct {
	conn   deadlineWriter
	header http.Header
	// onWriteError fires the first time a write to the client fails, which is
	// the other way a disconnect becomes visible: watchPeerGone notices a peer
	// that closed its side, and this notices one that has stopped reading
	// ours. Either way the request being served on its behalf should stop.
	onWriteError func()
	written      bool
	failed       bool
	status       int
}

func newConnResponseWriter(conn net.Conn, onWriteError func()) *connResponseWriter {
	return &connResponseWriter{
		conn:         deadlineWriter{conn: conn},
		header:       http.Header{},
		onWriteError: onWriteError,
		status:       http.StatusOK,
	}
}

// writeAll writes to the client, reporting the first failure exactly once.
func (w *connResponseWriter) writeAll(p []byte) (int, error) {
	n, err := w.conn.Write(p)
	if err != nil && !w.failed {
		w.failed = true
		if w.onWriteError != nil {
			w.onWriteError()
		}
	}
	return n, err
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
	_, _ = w.writeAll([]byte(b.String()))
}

func (w *connResponseWriter) Write(p []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	return w.writeAll(p)
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
