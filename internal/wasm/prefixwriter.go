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
// than once per write. A partial line is flushed when the instance closes.
type prefixWriter struct {
	mu     sync.Mutex
	out    io.Writer
	prefix string
	buf    []byte
	// err holds the first write failure. A guest that keeps writing into a
	// broken pipe should learn about it rather than see every call succeed.
	err error
}

// maxPrefixLineBytes bounds one buffered line. The guest chooses how much it
// writes; the host chooses how much it will hold.
const maxPrefixLineBytes = 64 << 10

func newPrefixWriter(out io.Writer, plugin string) *prefixWriter {
	return &prefixWriter{out: out, prefix: fmt.Sprintf("[plugin %s stdio] ", plugin)}
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	total := len(p)
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			w.appendLocked(p)
			break
		}
		w.appendLocked(p[:idx])
		w.flushLocked()
		p = p[idx+1:]
	}
	// The guest's write always "succeeded" in full — a partial count would
	// make a WASI writer retry bytes this side has already accounted for.
	return total, w.err
}

// appendLocked copies at most the remaining line budget at a time, flushing
// whenever it runs out.
//
// The bound used to be checked AFTER appending all of p, so a single
// newline-free guest write of any size was allocated on the host in full and
// only then noticed — a 64 KiB limit that a guest could ignore by writing
// 64 MiB at once. The budget has to be enforced before the copy, not after.
func (w *prefixWriter) appendLocked(p []byte) {
	for len(p) > 0 {
		room := maxPrefixLineBytes - len(w.buf)
		if room <= 0 {
			w.flushLocked()
			room = maxPrefixLineBytes
		}
		n := min(room, len(p))
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
		if len(w.buf) >= maxPrefixLineBytes {
			w.flushLocked()
		}
	}
}

// Flush emits any bytes the guest left without a trailing newline. Called when
// the instance closes: a guest's last line is often its most interesting one,
// and it used to be discarded because the writer was never retained.
func (w *prefixWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushLocked()
}

func (w *prefixWriter) flushLocked() {
	if len(w.buf) == 0 {
		return
	}
	_, err := fmt.Fprintf(w.out, "%s%s\n", w.prefix, escapeControl(w.buf))
	if err != nil && w.err == nil {
		w.err = err
	}
	w.buf = w.buf[:0]
}

const hexDigits = "0123456789abcdef"

// escapeControl replaces the bytes a terminal ACTS on rather than shows.
//
// A prefix is not attribution on its own: a guest line containing a carriage
// return redraws over it, and an ESC introduces cursor movement and colour
// that can forge a whole screen of host output. Tab is left alone — it cannot
// move the cursor backwards and log lines legitimately contain it.
func escapeControl(b []byte) []byte {
	needs := false
	for _, c := range b {
		if (c < 0x20 && c != '\t') || c == 0x7f {
			needs = true
			break
		}
	}
	if !needs {
		return b
	}
	out := make([]byte, 0, len(b)+8)
	for _, c := range b {
		if (c < 0x20 && c != '\t') || c == 0x7f {
			out = append(out, '\\', 'x', hexDigits[c>>4], hexDigits[c&0x0f])
			continue
		}
		out = append(out, c)
	}
	return out
}
