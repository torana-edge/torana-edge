package wasm

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// prefixWriter attributes every line a guest writes to its own stdout/stderr.
//
// A plugin granted env.log gets a real writer rather than io.Discard. Handing
// it the host's os.Stdout directly let it emit unattributed bytes into the
// operator's log, which is the one place an operator looks to find out what a
// plugin did — so a guest could forge lines indistinguishable from the host's.
//
// Lines are buffered until a newline so a prefix lands once per line rather
// than once per write, and a partial line is flushed with the rest.
type prefixWriter struct {
	mu     sync.Mutex
	out    io.Writer
	prefix string
	buf    bytes.Buffer
}

// maxPrefixLineBytes bounds one buffered line, so a guest writing without ever
// emitting a newline cannot grow host memory without limit.
const maxPrefixLineBytes = 64 << 10

func newPrefixWriter(out io.Writer, plugin string) *prefixWriter {
	return &prefixWriter{out: out, prefix: fmt.Sprintf("[plugin %s stdio] ", plugin)}
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	total := len(p)
	for {
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			w.buf.Write(p)
			if w.buf.Len() > maxPrefixLineBytes {
				w.flushLocked()
			}
			return total, nil
		}
		w.buf.Write(p[:idx])
		w.flushLocked()
		p = p[idx+1:]
	}
}

func (w *prefixWriter) flushLocked() {
	if w.buf.Len() == 0 {
		return
	}
	_, _ = fmt.Fprintf(w.out, "%s%s\n", w.prefix, w.buf.String())
	w.buf.Reset()
}
