package wasm

import (
	"errors"
	"strings"
	"testing"
)

type failingWriter struct{ n int }

func (f *failingWriter) Write(p []byte) (int, error) {
	f.n++
	return 0, errors.New("pipe closed")
}

func TestPrefixWriterAttributesEveryLine(t *testing.T) {
	var out strings.Builder
	w := newPrefixWriter(&out, "p")
	if _, err := w.Write([]byte("one\ntwo\n")); err != nil {
		t.Fatal(err)
	}
	want := "[plugin p stdio] one\n[plugin p stdio] two\n"
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}

// A line delivered across several writes is still one line.
func TestPrefixWriterJoinsFragmentedLines(t *testing.T) {
	var out strings.Builder
	w := newPrefixWriter(&out, "p")
	for _, frag := range []string{"hel", "lo wo", "rld"} {
		if _, err := w.Write([]byte(frag)); err != nil {
			t.Fatal(err)
		}
	}
	if out.String() != "" {
		t.Fatalf("emitted %q before the line was terminated", out.String())
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	if want := "[plugin p stdio] hello world\n"; out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}

func TestPrefixWriterEmitsEmptyLines(t *testing.T) {
	var out strings.Builder
	w := newPrefixWriter(&out, "p")
	if _, err := w.Write([]byte("\n\na\n")); err != nil {
		t.Fatal(err)
	}
	// An empty line carries no bytes to attribute, so it is not emitted as a
	// bare prefix; what matters is that it does not swallow the line after it.
	if want := "[plugin p stdio] a\n"; out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}

// The finding: the bound was checked AFTER appending all of p, so one
// newline-free guest write of any size was allocated on the host in full.
func TestPrefixWriterBoundsASingleOversizedWrite(t *testing.T) {
	var out strings.Builder
	w := newPrefixWriter(&out, "p")
	huge := strings.Repeat("x", 8*maxPrefixLineBytes+123)
	if _, err := w.Write([]byte(huge)); err != nil {
		t.Fatal(err)
	}
	if len(w.buf) > maxPrefixLineBytes {
		t.Errorf("buffer holds %d bytes after one write, bound is %d", len(w.buf), maxPrefixLineBytes)
	}
	if cap(w.buf) > 2*maxPrefixLineBytes {
		t.Errorf("buffer capacity grew to %d for a bound of %d; the guest is "+
			"choosing how much host memory to use", cap(w.buf), maxPrefixLineBytes)
	}
	w.Flush()
	if got := len(out.String()); got < len(huge) {
		t.Errorf("emitted %d bytes, dropped part of a %d-byte write", got, len(huge))
	}
}

// A guest's last line is often the one explaining why it stopped. It used to
// be discarded, because the writer was never retained or flushed.
func TestPrefixWriterFlushesAFinalUnterminatedLine(t *testing.T) {
	var out strings.Builder
	w := newPrefixWriter(&out, "p")
	if _, err := w.Write([]byte("panic: last words")); err != nil {
		t.Fatal(err)
	}
	if out.String() != "" {
		t.Fatalf("emitted %q before flush", out.String())
	}
	w.Flush()
	if want := "[plugin p stdio] panic: last words\n"; out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
	w.Flush() // idempotent
	if got := strings.Count(out.String(), "last words"); got != 1 {
		t.Errorf("a second Flush re-emitted the line %d times", got)
	}
}

func TestPrefixWriterReportsWriteFailures(t *testing.T) {
	f := &failingWriter{}
	w := newPrefixWriter(f, "p")
	if _, err := w.Write([]byte("a\n")); err == nil {
		t.Fatal("a failed write to the operator's log was reported as success")
	}
	if _, err := w.Write([]byte("b\n")); err == nil {
		t.Error("a later write did not surface the earlier failure")
	}
}

// A prefix is not attribution on its own: a carriage return redraws over it
// and an ESC can forge a screen of host output.
func TestPrefixWriterEscapesTerminalControlBytes(t *testing.T) {
	var out strings.Builder
	w := newPrefixWriter(&out, "p")
	if _, err := w.Write([]byte("safe\r[route] forged\x1b[31m\x00\x7f\tkept\n")); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, raw := range []string{"\r", "\x1b", "\x00", "\x7f"} {
		if strings.Contains(got, raw) {
			t.Errorf("control byte %q reached the operator's log: %q", raw, got)
		}
	}
	for _, esc := range []string{`\x0d`, `\x1b`, `\x00`, `\x7f`} {
		if !strings.Contains(got, esc) {
			t.Errorf("escape %s missing from %q", esc, got)
		}
	}
	if !strings.Contains(got, "\tkept") {
		t.Errorf("tab was escaped although it cannot move the cursor: %q", got)
	}
	if !strings.HasPrefix(got, "[plugin p stdio] ") {
		t.Errorf("line lost its prefix: %q", got)
	}
}
